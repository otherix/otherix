// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cloudinit"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagImageURL         = "image-url"
	flagImageSHA256      = "image-sha256"
	flagArch             = "arch"
	flagFirmware         = "firmware"
	flagFirmwareID       = "firmware-id"
	flagFormat           = "format"
	flagDiskGiB          = "disk-gib"
	flagPool             = "pool"
	flagNode             = "node"
	flagNetwork          = "network"
	flagVCPUs            = "vcpus"
	flagMemoryMB         = "memory-mb"
	flagCloudInitPath    = "cloud-init"
	flagCloudInitDisable = "no-cloud-init"

	defaultVCPUs    = 2
	defaultMemoryMB = 2048
	defaultWaitTO   = 5 * time.Minute
)

func newCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new VM (starts pending).",
		Long: `Creates the VM, which starts in the pending phase, and returns
immediately. The VM name is the sole positional argument (globally
unique). A CP-side reconcile loop binds the VM to a (node, pool) once
its dependencies are ready; pass --wait to block until the VM is
running. The VM uuid is minted on the CP and reused by the agent.

The VM is created directly from an image source — there is no template
entity. --image-url and --arch are required; the server downloads and
caches the image into the target pool. --image-sha256 pins the expected
digest, --firmware / --firmware-id select firmware (name or uuid),
--format and --disk-gib size the root disk.

Example:
  otherix vm create web-1 --image-url https://example.com/ubuntu.qcow2 \
    --arch arm64 --vcpus 2 --memory-mb 2048

--pool accepts either a pool name or a per-instance UUID literal
(multi-instance carve-out). The CLI forwards the raw string; the server
resolves it.

Multi-instance pools + scheduler:
  - --pool is optional. When omitted, the server uses the cluster
    default-pool reference (configured via 'otherix cluster
    set-default-pool'). Missing default returns 400
    default_pool_not_set.
  - --node is an optional placement hint: when set, the scheduler
    pins the VM to exactly that node; mismatch (pool not present on
    the requested node) returns 409 pool_not_on_node.

--network is optional: a bridge network name or uuid. When set the VM
gets one NIC attached to that network's bridge (the CP mints a
52:54:00 QEMU MAC; the guest configures its own IP). Non-bridge
network types are rejected with 400. When omitted the VM has no NIC
and the agent falls back to legacy SLIRP networking.`,
		Args: cobra.ExactArgs(1),
		RunE: runCreate,
	}

	cmd.Flags().String(flagImageURL, "", "source image URL to download and boot from (required)")
	cmd.Flags().String(flagImageSHA256, "", "expected sha256 of the image (optional; verified after download)")
	cmd.Flags().String(flagArch, "", "VM architecture: amd64 or arm64 (required)")
	cmd.Flags().String(flagFirmware, "", "firmware name (optional; mutually exclusive with --firmware-id)")
	cmd.Flags().String(flagFirmwareID, "", "firmware uuid (optional; mutually exclusive with --firmware)")
	cmd.Flags().String(flagFormat, "", "image disk format, e.g. qcow2 or raw (optional; server default applies)")
	cmd.Flags().Int(flagDiskGiB, 0, "root disk size in GiB (optional; defaults to the image's virtual size)")
	cmd.Flags().String(flagPool, "", "storage pool name or uuid (optional; cluster default used when empty)")
	cmd.Flags().String(flagNode, "", "explicit placement hint — node name or uuid (optional)")
	cmd.Flags().String(flagNetwork, "", "bridge network to attach one NIC to — network name or uuid (optional)")
	cmd.Flags().Int(flagVCPUs, defaultVCPUs, "vCPU count (1..128)")
	cmd.Flags().Int(flagMemoryMB, defaultMemoryMB, "memory in MiB (128..524288)")
	cmd.Flags().String(flagCloudInitPath, "",
		"path to a `#cloud-config` YAML; use '-' to read stdin. Mutually exclusive with --no-cloud-init.")
	cmd.Flags().Bool(flagCloudInitDisable, false,
		"explicitly disable cloud-init for this VM. Mutually exclusive with --cloud-init.")
	cmd.Flags().Bool(flagWait, false, "block until the VM reaches the running phase")
	cmd.Flags().Duration(flagWaitTimeout, defaultWaitTO, "max time to wait when --wait is set")

	return cmd
}

// createFlags collects the strongly-typed inputs runCreate needs.
// Extracting the parse step keeps runCreate's cyclomatic complexity
// inside the linter cap.
type createFlags struct {
	name              string
	imageURL          string
	imageSHA256       string
	arch              string
	firmware          string
	firmwareID        string
	format            string
	diskGiB           int
	pool              string
	node              string
	network           string
	vcpus             int
	memoryMB          int
	cloudInitUserData *string
	cloudInitDisabled bool
	wait              bool
	timeout           time.Duration
}

func parseCreateFlags(cmd *cobra.Command) (createFlags, error) {
	var f createFlags
	var err error
	if err = parseImageFlags(cmd, &f); err != nil {
		return f, err
	}
	if f.pool, err = cmd.Flags().GetString(flagPool); err != nil {
		return f, err
	}
	if f.node, err = cmd.Flags().GetString(flagNode); err != nil {
		return f, err
	}
	if f.network, err = cmd.Flags().GetString(flagNetwork); err != nil {
		return f, err
	}
	if f.vcpus, err = cmd.Flags().GetInt(flagVCPUs); err != nil {
		return f, err
	}
	if f.memoryMB, err = cmd.Flags().GetInt(flagMemoryMB); err != nil {
		return f, err
	}
	if err = parseCloudInitFlags(cmd, &f); err != nil {
		return f, err
	}
	if f.wait, err = cmd.Flags().GetBool(flagWait); err != nil {
		return f, err
	}
	if f.timeout, err = cmd.Flags().GetDuration(flagWaitTimeout); err != nil {
		return f, err
	}
	return f, nil
}

// parseImageFlags reads the image-source flags onto f. --image-url and
// --arch are required; --firmware / --firmware-id are mutually
// exclusive. Extracted from parseCreateFlags to keep that orchestrator
// inside the gocyclo cap.
func parseImageFlags(cmd *cobra.Command, f *createFlags) error {
	var err error
	if f.imageURL, err = requireStringFlag(cmd, flagImageURL); err != nil {
		return err
	}
	if f.arch, err = requireStringFlag(cmd, flagArch); err != nil {
		return err
	}
	if f.imageSHA256, err = cmd.Flags().GetString(flagImageSHA256); err != nil {
		return err
	}
	if f.firmware, err = cmd.Flags().GetString(flagFirmware); err != nil {
		return err
	}
	if f.firmwareID, err = cmd.Flags().GetString(flagFirmwareID); err != nil {
		return err
	}
	if f.firmware != "" && f.firmwareID != "" {
		return fmt.Errorf("--%s and --%s are mutually exclusive", flagFirmware, flagFirmwareID)
	}
	if f.format, err = cmd.Flags().GetString(flagFormat); err != nil {
		return err
	}
	if f.diskGiB, err = cmd.Flags().GetInt(flagDiskGiB); err != nil {
		return err
	}
	return nil
}

// parseCloudInitFlags reads --no-cloud-init and --cloud-init onto f and
// enforces their mutual exclusion. Extracted from parseCreateFlags to
// keep that orchestrator inside the gocyclo cap.
func parseCloudInitFlags(cmd *cobra.Command, f *createFlags) error {
	var err error
	if f.cloudInitDisabled, err = cmd.Flags().GetBool(flagCloudInitDisable); err != nil {
		return err
	}
	if f.cloudInitUserData, err = readCloudInitFlag(cmd, flagCloudInitPath); err != nil {
		return err
	}
	if f.cloudInitUserData != nil && f.cloudInitDisabled {
		return fmt.Errorf("--%s and --%s are mutually exclusive", flagCloudInitPath, flagCloudInitDisable)
	}
	return nil
}

// readCloudInitFlag turns the `--cloud-init=<path|->` flag into the
// resolved YAML content. Empty flag returns nil; non-empty reads
// (file or stdin) and validates (best-effort) through the shared
// cloudinit package so the contract (warnings to stderr, parse errors
// bubble up) stays uniform across resources.
func readCloudInitFlag(cmd *cobra.Command, name string) (*string, error) {
	raw, err := cmd.Flags().GetString(name)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	data, err := cloudinit.ReadSource(raw)
	if err != nil {
		return nil, fmt.Errorf("--%s: %w", name, err)
	}
	warnings, err := cloudinit.Validate(data)
	if err != nil {
		return nil, fmt.Errorf("--%s: %w", name, err)
	}
	stderr := cmd.ErrOrStderr()
	if stderr == nil {
		stderr = os.Stderr
	}
	for _, w := range warnings {
		_, _ = fmt.Fprintf(stderr, "warning: --%s: %s\n", name, w)
	}
	out := string(data)
	return &out, nil
}

func runCreate(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	name := args[0]
	if name == "" {
		return fmt.Errorf("vm name is required")
	}
	f, err := parseCreateFlags(cmd)
	if err != nil {
		return err
	}
	f.name = name

	req := cpclient.CreateVMRequest{
		Name:              f.name,
		ImageURL:          f.imageURL,
		ImageSHA256:       f.imageSHA256,
		Arch:              f.arch,
		Firmware:          f.firmware,
		FirmwareID:        f.firmwareID,
		Format:            f.format,
		DiskGiB:           f.diskGiB,
		Pool:              f.pool,
		Network:           f.network,
		VCPUs:             f.vcpus,
		MemoryMB:          f.memoryMB,
		UserData:          f.cloudInitUserData,
		CloudInitDisabled: f.cloudInitDisabled,
	}
	if f.node != "" {
		req.Node = &f.node
	}
	created, _, err := c.CreateVM(cmd.Context(), req)
	if err != nil {
		return classifyError(err)
	}

	printf(cmd, "created vm=%s status=%s\n", created.Name, created.Status.Phase)

	if !f.wait {
		return nil
	}

	if err := waitForVMPhase(cmd.Context(), cmd, c, created.Name, f.timeout); err != nil {
		return classifyError(err)
	}
	printf(cmd, "vm running name=%s\n", created.Name)
	return nil
}
