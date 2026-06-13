// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cloudinit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
	yaml "gopkg.in/yaml.v3"
)

// DefaultDiskSize is the ISO image size in bytes used when Builder.Size
// is zero. 10 MiB is generous for a NoCloud cidata payload (typical
// user-data + meta-data are under a kilobyte); ISO9660 minimum
// usable size is around 350 KiB but go-diskfs needs slack for its
// workspace. Bumping past the default is cheap (sparse file
// allocation) and safe.
const DefaultDiskSize int64 = 10 * 1024 * 1024

// VolumeLabel is the ISO9660 volume identifier the NoCloud datasource
// looks for. cloud-init treats a CD-ROM with this label as the NoCloud
// seed source. Constant by spec — must not be configurable.
const VolumeLabel = "cidata"

// ErrEmptyHostname signals that the caller passed an empty Hostname
// although hostname injection was requested. The package refuses to
// generate a cidata ISO with meta-data missing local-hostname since
// guest behaviour without one is implementation-defined.
var ErrEmptyHostname = errors.New("cloudinit: empty hostname")

// Builder composes a NoCloud cidata ISO. Build assembles the
// ISO9660 filesystem, writes meta-data + user-data (+ optional
// network-config), and returns the closed ISO image path.
type Builder struct {
	// Hostname feeds local-hostname in meta-data and (if missing
	// from UserData) is injected as a top-level `hostname:` key.
	// Required — see ErrEmptyHostname.
	Hostname string

	// UserData is raw `#cloud-config` YAML. CP-side resolves
	// vm.user_data (no template fallback); the agent
	// just consumes the blob. Empty UserData is acceptable —
	// the resulting ISO carries only meta-data, which is enough
	// to satisfy the NoCloud datasource (cloud-init will boot
	// the guest with no extra config beyond hostname).
	UserData []byte

	// NetworkData is optional `#cloud-config` network YAML. When
	// non-empty, writes /network-config; otherwise omitted and
	// cloud-init falls back to DHCP discovery.
	NetworkData []byte

	// Size overrides the ISO image size in bytes. Zero → DefaultDiskSize.
	Size int64
}

// Build writes the cidata ISO to outputPath. The parent directory must
// exist; existing files at outputPath are overwritten. Returns the
// constructed path (== outputPath) on success.
func (b *Builder) Build(outputPath string) (string, error) {
	if err := b.validate(outputPath); err != nil {
		return "", err
	}
	if err := os.RemoveAll(outputPath); err != nil {
		return "", fmt.Errorf("cloudinit: remove existing: %w", err)
	}
	size := b.Size
	if size <= 0 {
		size = DefaultDiskSize
	}
	d, err := diskfs.Create(outputPath, size, diskfs.SectorSizeDefault)
	if err != nil {
		return "", fmt.Errorf("cloudinit: create disk: %w", err)
	}
	d.LogicalBlocksize = 2048
	// The ISO carries guest secrets (user-data may contain passwords
	// and SSH keys), so it must be owner-only like the agent's cert
	// key material. diskfs.Create makes the backing file 0o666 (0644
	// after the default umask) and the file is still empty here;
	// tightening the mode before
	// populateAndFinalize means secret bytes are only ever written
	// into an already-0600 file. The chmod does not affect the open
	// handle diskfs holds, so subsequent writes succeed.
	if err := os.Chmod(outputPath, 0o600); err != nil {
		_ = d.Close()
		return "", fmt.Errorf("cloudinit: chmod cidata: %w", err)
	}
	if err := b.populateAndFinalize(d); err != nil {
		_ = d.Close()
		return "", err
	}
	if err := d.Close(); err != nil {
		return "", fmt.Errorf("cloudinit: close iso: %w", err)
	}
	return outputPath, nil
}

// validate runs the pre-flight checks that do not require any disk
// allocation: hostname presence, output path shape, parent dir
// existence. Extracted so Build stays under gocyclo 15.
func (b *Builder) validate(outputPath string) error {
	if b.Hostname == "" {
		return ErrEmptyHostname
	}
	if outputPath == "" {
		return errors.New("cloudinit: empty output path")
	}
	if _, err := os.Stat(filepath.Dir(outputPath)); err != nil {
		return fmt.Errorf("cloudinit: parent directory: %w", err)
	}
	return nil
}

// populateAndFinalize assembles the filesystem, writes the three
// NoCloud files, and finalizes the ISO with the cidata volume label. The
// caller owns disk lifecycle (Create / Close); this helper only
// touches the filesystem layer.
func (b *Builder) populateAndFinalize(d *disk.Disk) error {
	spec := disk.FilesystemSpec{
		Partition:   0,
		FSType:      filesystem.TypeISO9660,
		VolumeLabel: VolumeLabel,
		WorkDir:     "",
	}
	fs, err := d.CreateFilesystem(spec)
	if err != nil {
		return fmt.Errorf("cloudinit: create filesystem: %w", err)
	}
	userData, err := injectHostname(b.UserData, b.Hostname)
	if err != nil {
		return fmt.Errorf("cloudinit: hostname injection: %w", err)
	}
	metaData, err := buildMetaData(b.Hostname)
	if err != nil {
		return err
	}
	if err := writeFile(fs, "/meta-data", metaData); err != nil {
		return err
	}
	if err := writeFile(fs, "/user-data", userData); err != nil {
		return err
	}
	if len(b.NetworkData) > 0 {
		if err := writeFile(fs, "/network-config", b.NetworkData); err != nil {
			return err
		}
	}
	iso, ok := fs.(*iso9660.FileSystem)
	if !ok {
		return fmt.Errorf("cloudinit: unexpected filesystem type %T", fs)
	}
	// RockRidge + Joliet preserve full POSIX filenames in the on-disk
	// directory entries. cloud-init's NoCloud datasource matches the
	// canonical names `meta-data` / `user-data` / `network-config` —
	// plain ISO9660 Level 1 would truncate to `META_DAT` (8.3 +
	// underscore substitution) and the guest would silently ignore the
	// seed. Without these extensions the cidata ISO mounts on the
	// guest but cloud-init never picks up any data.
	if err := iso.Finalize(iso9660.FinalizeOptions{
		VolumeIdentifier: VolumeLabel,
		RockRidge:        true,
		DeepDirectories:  true,
	}); err != nil {
		return fmt.Errorf("cloudinit: finalize iso: %w", err)
	}
	return nil
}

func writeFile(fs filesystem.FileSystem, name string, content []byte) error {
	f, err := fs.OpenFile(name, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return fmt.Errorf("cloudinit: open %s: %w", name, err)
	}
	if _, err := f.Write(content); err != nil {
		return fmt.Errorf("cloudinit: write %s: %w", name, err)
	}
	return nil
}

// buildMetaData renders the NoCloud meta-data document. The hostname is
// yaml-encoded (never interpolated) so a name carrying a newline or YAML
// metacharacter cannot inject extra top-level keys (audit R2-M5).
func buildMetaData(hostname string) ([]byte, error) {
	type metaData struct {
		InstanceID    string `yaml:"instance-id"`
		LocalHostname string `yaml:"local-hostname"`
	}
	b, err := yaml.Marshal(metaData{InstanceID: "iid-otherix-" + hostname, LocalHostname: hostname})
	if err != nil {
		return nil, fmt.Errorf("cloudinit: marshal meta-data: %w", err)
	}
	return b, nil
}
