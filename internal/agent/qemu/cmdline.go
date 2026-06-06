// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package qemu provides agent-side QEMU process management: command-line
// construction (architecture-aware), QMP control over the per-VM monitor
// socket, and process supervision via pidfile + kill -0 + QMP probe.
//
// Iteration 1 covers the minimum needed for VM create / delete:
// query-status, system_powerdown, quit. Future iterations layer
// pause/resume, migrate, query-block on top of the same QMPClient.
package qemu

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/netfabric"
)

// Architecture identifies a guest CPU architecture. The wire and
// configuration form is the Go GOARCH string (amd64, arm64) so values
// can flow between runtime detection, CP requests, and stored metadata
// without translation.
type Architecture string

// Architecture values supported by the agent. Cross-arch guests are out
// of scope — host arch must match guest arch.
const (
	ArchAMD64 Architecture = "amd64"
	ArchARM64 Architecture = "arm64"
)

// DetectAccelerator returns "kvm" when /dev/kvm is present (Linux host
// with KVM module loaded — bare metal, M3+ Lima vz, etc.) and "tcg"
// otherwise (M1/M2 Lima vz, Intel Macs without nested virt). qemu's
// `-accel` flag accepts a single accelerator name only — the colon-
// separated fallback form (`kvm:tcg`) only works as `-machine
// accel=kvm:tcg`, not as `-accel kvm:tcg`.
func DetectAccelerator() string {
	if _, err := os.Stat("/dev/kvm"); err == nil {
		return "kvm"
	}
	return "tcg"
}

// HostArch returns the architecture of the host the agent runs on.
// Iteration 1 only supports same-arch guests — cross-arch emulation is
// possible (TCG) but out of scope.
func HostArch() Architecture {
	switch runtime.GOARCH {
	case "amd64":
		return ArchAMD64
	case "arm64":
		return ArchARM64
	default:
		return Architecture(runtime.GOARCH)
	}
}

// Binary returns the qemu-system-* binary name for arch.
func Binary(arch Architecture) (string, error) {
	switch arch {
	case ArchAMD64:
		return "qemu-system-x86_64", nil
	case ArchARM64:
		return "qemu-system-aarch64", nil
	default:
		return "", fmt.Errorf("unsupported architecture: %s", arch)
	}
}

// VMSpec carries everything BuildArgs needs to construct a qemu
// command line. Path fields are absolute; the caller is responsible
// for creating their parent directories.
type VMSpec struct {
	Name            string
	UUID            uuid.UUID
	VCPUs           int
	MemoryMB        int
	Architecture    Architecture
	Accelerator     string // "kvm" or "tcg"; see DetectAccelerator
	DiskPath        string
	QMPSocket       string
	ConsoleSocket   string
	PIDFile         string
	AArch64Firmware string // required when Architecture==ArchARM64
	// CidataPath, if non-empty, attaches the NoCloud cidata ISO as a
	// read-only virtio drive. cloud-init's NoCloud datasource scans
	// every block device for the `cidata` volume label so virtio
	// presentation works alongside the OS disk without media-type
	// gymnastics. Empty value skips attachment entirely.
	CidataPath string
	// NICs are the CP-declared network interfaces to attach as tap
	// netdevs, each bridged on the host by the netfabric package. An
	// empty slice falls back to a single user-mode SLIRP netdev
	// (legacy / no-NIC VMs and smoke tests).
	NICs []netfabric.NIC
}

// qemuModel maps a netfabric NIC model name to the QEMU -device name.
// An empty model defaults to virtio. ValidateModel gates the input, so
// only the recognised set reaches this mapper.
func qemuModel(model string) string {
	switch model {
	case "", "virtio":
		return "virtio-net-pci"
	default:
		return model
	}
}

// networkArgs returns the -netdev / -device pairs for the VM's NICs. An
// empty slice yields the legacy user-mode SLIRP pair on net0 so no-NIC
// VMs and smoke tests keep working. Otherwise each NIC becomes a tap
// netdev bridged on the host, indexed net0..netN by ascending
// DeviceOrder (stable for equal orders).
func networkArgs(nics []netfabric.NIC) []string {
	if len(nics) == 0 {
		return []string{
			"-netdev", "user,id=net0",
			"-device", "virtio-net-pci,netdev=net0",
		}
	}

	sorted := make([]netfabric.NIC, len(nics))
	copy(sorted, nics)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].DeviceOrder < sorted[j].DeviceOrder
	})

	args := make([]string, 0, len(sorted)*4)
	for i, nic := range sorted {
		id := "net" + strconv.Itoa(i)
		args = append(args,
			"-netdev", fmt.Sprintf("tap,id=%s,ifname=%s,script=no,downscript=no", id, nic.TapName()),
			"-device", fmt.Sprintf("%s,netdev=%s,mac=%s", qemuModel(nic.Model), id, nic.MAC),
		)
	}
	return args
}

// BuildArgs returns the qemu argument vector (excluding the binary
// itself) for spec. Common flags for both architectures cover machine
// identity, virtio devices for disk/net, the per-VM QMP and serial
// sockets, the pidfile, and -daemonize so the parent exits cleanly
// after the child detaches.
//
// Acceleration is single-valued: spec.Accelerator must be "kvm" or
// "tcg". Manager picks one at startup via DetectAccelerator so M1/M2
// Lima (no /dev/kvm) gets tcg software emulation while bare-metal
// Linux and M3+ Lima vz get kvm. The fallback-list form
// (`kvm:tcg`) only works in `-machine accel=` syntax, not in `-accel`.
func BuildArgs(spec VMSpec) ([]string, error) {
	if err := validateSpec(spec); err != nil {
		return nil, err
	}

	args := []string{
		"-name", spec.Name,
		"-uuid", spec.UUID.String(),
		"-smp", strconv.Itoa(spec.VCPUs),
		"-m", strconv.Itoa(spec.MemoryMB),
		"-accel", spec.Accelerator,
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio,cache=none", spec.DiskPath),
		"-serial", fmt.Sprintf("unix:%s,server,nowait", spec.ConsoleSocket),
		"-qmp", fmt.Sprintf("unix:%s,server,nowait", spec.QMPSocket),
		"-pidfile", spec.PIDFile,
		"-daemonize",
		// -display none disables the graphical display only. -nographic
		// would also redirect serial/monitor to stdio, which conflicts
		// with -daemonize on qemu 8.x ("cannot be used with -daemonize").
		// Serial and QMP are already wired to explicit unix sockets above.
		"-display", "none",
	}
	if spec.CidataPath != "" {
		args = append(args,
			"-drive", fmt.Sprintf("file=%s,format=raw,if=virtio,readonly=on", spec.CidataPath),
		)
	}

	args = append(args, networkArgs(spec.NICs)...)

	switch spec.Architecture {
	case ArchAMD64:
		// q35 default machine type, host CPU features for best perf when KVM
		// is available; falls back automatically under TCG.
		args = append(args, "-cpu", "host")
	case ArchARM64:
		// virt machine + max CPU model + UEFI firmware are mandatory on
		// aarch64 — no legacy BIOS path exists.
		args = append(args,
			"-machine", "virt",
			"-cpu", "max",
			"-bios", spec.AArch64Firmware,
		)
	default:
		return nil, fmt.Errorf("unsupported architecture: %s", spec.Architecture)
	}

	return args, nil
}

func validateSpec(spec VMSpec) error {
	if spec.Name == "" {
		return fmt.Errorf("name is required")
	}
	if spec.UUID == uuid.Nil {
		return fmt.Errorf("uuid is required")
	}
	if spec.VCPUs < 1 {
		return fmt.Errorf("vcpus must be >= 1")
	}
	if spec.MemoryMB < 128 {
		return fmt.Errorf("memory_mb must be >= 128")
	}
	if spec.DiskPath == "" || spec.QMPSocket == "" || spec.ConsoleSocket == "" || spec.PIDFile == "" {
		return fmt.Errorf("disk_path, qmp_socket, console_socket, pid_file are all required")
	}
	if spec.Architecture == ArchARM64 && spec.AArch64Firmware == "" {
		return fmt.Errorf("aarch64 spec requires AArch64Firmware path")
	}
	if spec.Accelerator != "kvm" && spec.Accelerator != "tcg" {
		return fmt.Errorf("accelerator must be \"kvm\" or \"tcg\" (got %q)", spec.Accelerator)
	}
	return validateNICs(spec.NICs)
}

func validateNICs(nics []netfabric.NIC) error {
	for i, nic := range nics {
		if err := netfabric.ValidateMAC(nic.MAC); err != nil {
			return fmt.Errorf("nic %d: %v", i, err)
		}
		if err := netfabric.ValidateModel(nic.Model); err != nil {
			return fmt.Errorf("nic %d: %v", i, err)
		}
	}
	return nil
}
