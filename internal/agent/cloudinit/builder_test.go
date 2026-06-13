// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cloudinit

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// TestBuildMetaDataYAMLSafe drives R2-M5: a hostname carrying a newline must
// round-trip as a single quoted scalar, never injecting an extra top-level
// YAML key. The output must parse to EXACTLY two keys (instance-id,
// local-hostname) with local-hostname equal to the literal input.
func TestBuildMetaDataYAMLSafe(t *testing.T) {
	evil := "evil\nruncmd: [touch /pwned]"
	out, err := buildMetaData(evil)
	if err != nil {
		t.Fatalf("buildMetaData returned err = %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("yaml.Unmarshal(meta-data) err = %v:\n%s", err, out)
	}
	if got, want := len(m), 2; got != want {
		t.Fatalf("meta-data key count = %d, want %d (keys=%v):\n%s", got, want, keysOf(m), out)
	}
	if got, want := m["local-hostname"], evil; got != want {
		t.Errorf("local-hostname = %q, want %q", got, want)
	}
	if got, want := m["instance-id"], "iid-otherix-"+evil; got != want {
		t.Errorf("instance-id = %q, want %q", got, want)
	}
	if _, injected := m["runcmd"]; injected {
		t.Errorf("newline in hostname injected a runcmd key:\n%s", out)
	}
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// ISO9660 Primary Volume Descriptor starts at byte offset 32768
// (logical sector 16). Magic "CD001" sits at PVD+1 (PVD[0] is the
// volume descriptor type, 0x01 for PVD). VolumeIdentifier lives at
// PVD+40 for 32 bytes ASCII, space-padded but go-diskfs uses NUL
// padding in practice.
const (
	pvdOffset           = 32768
	pvdMagic            = "CD001"
	volIdentifierOffset = pvdOffset + 40
	volIdentifierLen    = 32
)

func TestBuilderBuild_VolumeLabelAndFiles(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "cidata.iso")

	b := &Builder{
		Hostname: "vm-test",
		UserData: []byte("#cloud-config\nusers:\n  - name: ubuntu\n"),
	}
	got, err := b.Build(out)
	if err != nil {
		t.Fatalf("Build returned err = %v", err)
	}
	if got != out {
		t.Errorf("Build returned %q, want %q", got, out)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read iso: %v", err)
	}
	if len(data) < pvdOffset+volIdentifierLen+volIdentifierOffset {
		t.Fatalf("iso too small: %d bytes", len(data))
	}
	if string(data[pvdOffset+1:pvdOffset+1+len(pvdMagic)]) != pvdMagic {
		t.Errorf("ISO9660 magic missing at offset %d", pvdOffset+1)
	}
	label := strings.TrimRight(string(data[volIdentifierOffset:volIdentifierOffset+volIdentifierLen]), " \x00")
	if label != VolumeLabel {
		t.Errorf("volume label = %q, want %q", label, VolumeLabel)
	}
	if !bytes.Contains(data, []byte("local-hostname: vm-test")) {
		t.Errorf("iso missing meta-data hostname marker")
	}
	if !bytes.Contains(data, []byte("name: ubuntu")) {
		t.Errorf("iso missing user-data marker")
	}
}

func TestBuilderBuildEmptyUserData(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "cidata.iso")
	b := &Builder{Hostname: "minimal-vm"}
	if _, err := b.Build(out); err != nil {
		t.Fatalf("Build returned err = %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read iso: %v", err)
	}
	if !bytes.Contains(data, []byte("hostname: minimal-vm")) {
		t.Errorf("empty user-data should inject hostname: marker")
	}
}

func TestBuilderBuildWithNetworkData(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "cidata.iso")
	b := &Builder{
		Hostname:    "net-vm",
		UserData:    []byte("#cloud-config\n"),
		NetworkData: []byte("version: 2\nethernets:\n  ens3: {dhcp4: true}\n"),
	}
	if _, err := b.Build(out); err != nil {
		t.Fatalf("Build: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read iso: %v", err)
	}
	if !bytes.Contains(data, []byte("ens3")) {
		t.Errorf("iso missing network-config marker")
	}
}

func TestBuilderBuildOmitsNetworkConfigWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "cidata.iso")
	b := &Builder{
		Hostname: "no-net-vm",
		UserData: []byte("#cloud-config\n"),
	}
	if _, err := b.Build(out); err != nil {
		t.Fatalf("Build: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read iso: %v", err)
	}
	if bytes.Contains(data, []byte("network-config")) {
		t.Errorf("iso contains network-config entry when NetworkData empty")
	}
}

func TestBuilderBuildISOMode0600(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "cidata.iso")
	b := &Builder{
		Hostname: "h",
		UserData: []byte("#cloud-config\nusers:\n  - name: secret-user\n"),
	}
	if _, err := b.Build(out); err != nil {
		t.Fatalf("Build returned err = %v", err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat iso: %v", err)
	}
	if got, want := fi.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Errorf("Build() ISO mode = %v, want %v", got, want)
	}
}

func TestBuilderBuildEmptyHostname(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "cidata.iso")
	b := &Builder{}
	_, err := b.Build(out)
	if !errors.Is(err, ErrEmptyHostname) {
		t.Errorf("err = %v, want ErrEmptyHostname", err)
	}
}
