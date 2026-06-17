// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/version"
)

// VMLister is the narrow vm.Manager surface the collector depends on.
// Constructed by the agent runtime; tests pass a fake.
type VMLister interface {
	List() []*vm.VM
}

// Collector returns the heartbeat report for the current tick. Implementations
// must be safe to call concurrently — the sender invokes Collect
// from a single goroutine, but tests may exercise the method in
// parallel.
type Collector interface {
	Collect(ctx context.Context) (Report, error)
}

// PoolReporter returns the per-pool observed-state slice the collector
// folds into HeartbeatRequest.pools. Implemented by the reconciler
// package; nil is allowed (yields zero pool reports) for test paths
// that don't need pool reconciliation wiring.
type PoolReporter interface {
	PoolReports() []PoolReport
}

// PoolImageLister returns the per-pool cached-image inventory the
// collector folds into each PoolReport.Images, plus a known flag. known is
// false when the agent could not enumerate the pool this tick (a transient
// error); the collector then marks PoolReport.ImagesUnavailable so the CP
// preserves the prior inventory rather than clearing it (audit R2-L11).
// Implemented by an adapter over vm.Manager.ListImages in the agent serve
// assembly; nil is allowed (yields no image inventory) for test paths and the
// legacy wiring.
type PoolImageLister interface {
	PoolImages(pool string) ([]PoolImageReport, bool)
}

// PoolSnapshotLister returns the per-pool cached snapshot-blob inventory the
// collector folds into each PoolReport.Snapshots, plus a known flag. known is
// false when the agent could not enumerate the pool this tick (a transient
// error); the collector then marks PoolReport.SnapshotsUnavailable so the CP
// preserves the prior inventory rather than clearing it. Implemented by an
// adapter over vm.Manager.ListSnapshots in the agent serve assembly; nil is
// allowed (yields no snapshot inventory) for test paths and the legacy wiring.
// Mirrors PoolImageLister.
type PoolSnapshotLister interface {
	PoolSnapshots(pool string) ([]PoolSnapshotReport, bool)
}

// VMReporter returns the per-VM observed-state slice the collector
// folds into HeartbeatRequest.vms. Implemented by the VM reconciler
// per L3 D3 — single ownership of observed VM state mirrors the
// pool reporter precedent. Nil disables the seam and the collector
// falls back to Manager.List() directly (legacy / test paths).
type VMReporter interface {
	VMReports() []VMReport
}

// NetworkReporter returns the per-network observed-state slice the
// collector folds into HeartbeatRequest.networks. Implemented by the
// network reconciler; nil is allowed (yields zero network reports) for
// test paths that don't need network reconciliation wiring.
type NetworkReporter interface {
	NetworkReports() []NetworkReport
}

// WireGuardReporter returns the agent's observed WG interface state the
// collector folds into HeartbeatRequest.wireguard. Implemented by the WG
// reconciler. Nil is allowed (yields a nil report) for test/legacy paths.
type WireGuardReporter interface {
	WireGuardReport() *WireGuardReport
}

// LinuxCollector reads host inventory from /proc, runs qemu-system-*
// --version best-effort, and asks the supplied VMLister for the live
// VM list. Designed for Linux (the only supported agent platform);
// other platforms fail-fast in New rather than at Collect time.
type LinuxCollector struct {
	procPath      string
	vms           VMLister
	vmReporter    VMReporter
	pools         PoolReporter
	poolImages    PoolImageLister
	poolSnapshots PoolSnapshotLister
	networks      NetworkReporter
	wireguard     WireGuardReporter
	migration     config.MigrationConfig
	qemu          config.QEMUConfig
	agentVersion  string
	architecture  string
	hostName      string
}

// CollectorDeps bundles the collector's construction-time inputs.
// procPath defaults to /proc; tests override it for synthetic
// fixtures. Pools may be nil — collector skips the pool-report field.
// VMReporter (L3 D3): when supplied, the collector uses it as the
// authoritative source for HeartbeatRequest.vms[] and keeps VMs only
// for resource accounting (sumRunningVMs). When nil, the collector
// falls back to VMs.List() for backward compatibility with tests.
type CollectorDeps struct {
	ProcPath      string
	VMs           VMLister
	VMReporter    VMReporter
	Pools         PoolReporter
	PoolImages    PoolImageLister
	PoolSnapshots PoolSnapshotLister
	Networks      NetworkReporter
	WireGuard     WireGuardReporter
	Migration     config.MigrationConfig
	QEMU          config.QEMUConfig
}

// NewLinux constructs a LinuxCollector. Returns an error on non-Linux
// hosts so the agent fails fast at boot rather than producing
// degraded heartbeats forever.
func NewLinux(deps CollectorDeps) (*LinuxCollector, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("heartbeat: LinuxCollector requires GOOS=linux, got %s", runtime.GOOS)
	}
	if deps.VMs == nil {
		return nil, fmt.Errorf("heartbeat: VMs lister is required")
	}
	procPath := deps.ProcPath
	if procPath == "" {
		procPath = "/proc"
	}
	host, _ := os.Hostname()
	return &LinuxCollector{
		procPath:      procPath,
		vms:           deps.VMs,
		vmReporter:    deps.VMReporter,
		pools:         deps.Pools,
		poolImages:    deps.PoolImages,
		poolSnapshots: deps.PoolSnapshots,
		networks:      deps.Networks,
		wireguard:     deps.WireGuard,
		migration:     deps.Migration,
		qemu:          deps.QEMU,
		agentVersion:  version.Current().Version,
		architecture:  archFromGo(runtime.GOARCH),
		hostName:      host,
	}, nil
}

// Collect builds the heartbeat report. Per-field failures (e.g.
// /proc/cpuinfo unreadable) degrade gracefully: required fields fall
// back to sentinel values that pass the receiver's validation
// ("unknown" for strings, 0 for counts) so the agent never blocks the
// liveness signal on host introspection gaps.
func (c *LinuxCollector) Collect(_ context.Context) (Report, error) {
	caps := NodeCapabilities{
		CPUFlags:     []string{},
		QEMUBinaries: map[string]string{},
	}

	model, flags, cores := c.readCPUInfo()
	caps.CPUModel = model
	if caps.CPUModel == "" {
		// /proc/cpuinfo on ARM64 has no "model name" field — fall
		// back so the receiver's required-field validator passes.
		caps.CPUModel = "unknown"
	}
	caps.CPUFlags = flags
	if cores > 0 {
		caps.CPUCoresTotal = clampToInt32(cores)
	} else {
		caps.CPUCoresTotal = clampToInt32(runtime.NumCPU())
	}

	memTotal := c.readMemTotalMib()
	caps.MemoryTotalMib = memTotal

	caps.KernelVersion = c.readKernelVersion()
	if caps.KernelVersion == "" {
		caps.KernelVersion = "unknown"
	}

	caps.QEMUBinaries, caps.QEMUVersion = c.readQemuInventory()
	if caps.QEMUVersion == "" {
		caps.QEMUVersion = "unknown"
	}

	caps.KvmAvailable = c.kvmAvailable()
	caps.NestedVirt = c.nestedVirtEnabled()

	usedCores, usedMib := c.sumRunningVMs()
	resources := NodeResources{
		CPUCoresAvailable:  clampNonNegative32(caps.CPUCoresTotal - usedCores),
		MemoryAvailableMib: clampNonNegative64(caps.MemoryTotalMib - usedMib),
	}
	// Root filesystem metrics. Errors are not propagated - a statfs
	// failure should not block the heartbeat liveness signal. Caller
	// carries the metric forward as NULL and the CP receiver preserves
	// existing system_disk pressure state.
	if total, avail, err := rootFilesystemStats(); err == nil {
		resources.SystemDiskTotalBytes = total
		resources.SystemDiskAvailableBytes = avail
	}

	migration := &MigrationCap{
		Host:           c.migration.Host,
		PortRangeStart: clampToInt32(c.migration.PortRangeStart),
		PortRangeEnd:   clampToInt32(c.migration.PortRangeEnd),
	}
	if migration.Host == "" {
		migration = nil
	}

	report := Report{
		AgentVersion: c.agentVersion,
		Architecture: c.architecture,
		Migration:    migration,
		Capabilities: caps,
		Resources:    resources,
		VMs:          c.buildVMReports(),
	}
	if c.pools != nil {
		report.Pools = c.pools.PoolReports()
		if c.poolImages != nil {
			// The PoolReporter owns reconciliation status; the image
			// inventory is a separate observation merged in per pool name.
			for i := range report.Pools {
				imgs, known := c.poolImages.PoolImages(report.Pools[i].Name)
				report.Pools[i].Images = imgs
				report.Pools[i].ImagesUnavailable = !known
			}
		}
		if c.poolSnapshots != nil {
			// Snapshot inventory is a separate observation merged in per pool
			// name, exactly like the image inventory above.
			for i := range report.Pools {
				snaps, known := c.poolSnapshots.PoolSnapshots(report.Pools[i].Name)
				report.Pools[i].Snapshots = snaps
				report.Pools[i].SnapshotsUnavailable = !known
			}
		}
	}
	if c.networks != nil {
		report.Networks = c.networks.NetworkReports()
	}
	if c.wireguard != nil {
		report.WireGuard = c.wireguard.WireGuardReport()
	}
	return report, nil
}

// buildVMReports prefers the reconciler-owned cache when supplied and
// falls back to a direct Manager.List() projection for legacy / test
// paths. Per L3 D3 the reconciler is the single owner of observed VM
// state once wired; the fallback exists so collector_test fixtures
// can stay decoupled from the reconciler package.
func (c *LinuxCollector) buildVMReports() []VMReport {
	if c.vmReporter != nil {
		return c.vmReporter.VMReports()
	}
	return c.collectVMs()
}

// readCPUInfo parses /proc/cpuinfo for model name, flags (from the first
// CPU block), and core count. Missing or unreadable file returns
// empty / 0 — caller falls back on runtime.NumCPU.
func (c *LinuxCollector) readCPUInfo() (model string, flags []string, cores int) {
	f, err := os.Open(filepath.Join(c.procPath, "cpuinfo"))
	if err != nil {
		return "", nil, 0
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		key, value, ok := splitKV(line)
		if !ok {
			continue
		}
		switch key {
		case "processor":
			cores++
		case "model name":
			if model == "" {
				model = value
			}
		case "flags", "Features":
			if len(flags) == 0 {
				flags = strings.Fields(value)
			}
		}
	}
	return model, flags, cores
}

// readMemTotalMib parses /proc/meminfo's MemTotal key. The value is
// reported in kB; convert to MiB. Missing or unreadable returns 0.
func (c *LinuxCollector) readMemTotalMib() int64 {
	f, err := os.Open(filepath.Join(c.procPath, "meminfo"))
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		key, value, ok := splitKV(line)
		if !ok {
			continue
		}
		if key != "MemTotal" {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return 0
		}
		kib, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return 0
		}
		return kib / 1024
	}
	return 0
}

// readKernelVersion preferentially uses the platform's uname syscall
// (see kernel_*.go for the per-platform implementation). Falls back
// to /proc/version if uname is not available or fails.
func (c *LinuxCollector) readKernelVersion() string {
	if v := kernelReleaseFromUname(); v != "" {
		return v
	}
	data, err := os.ReadFile(filepath.Join(c.procPath, "version"))
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(data))
	if len(fields) >= 3 {
		return fields[2]
	}
	return strings.TrimSpace(string(data))
}

// readQemuInventory exec's qemu-system-x86_64 and qemu-system-aarch64
// best-effort. Returns the binary→version map und the first found
// version string. Missing binaries are silently skipped.
func (c *LinuxCollector) readQemuInventory() (map[string]string, string) {
	out := map[string]string{}
	var first string
	for _, name := range []string{"qemu-system-x86_64", "qemu-system-aarch64"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		v := runQEMUVersion(path)
		if v == "" {
			continue
		}
		out[name] = v
		if first == "" {
			first = v
		}
	}
	return out, first
}

// runQEMUVersion parses `qemu-system-* --version` first line.
// Format: "QEMU emulator version 8.2.0 (...)". Returns the version
// token or an empty string if the command fails or the output is
// unexpected.
func runQEMUVersion(path string) string {
	cmd := exec.Command(path, "--version")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	line := strings.SplitN(string(output), "\n", 2)[0]
	fields := strings.Fields(line)
	for i, f := range fields {
		if f == "version" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// kvmAvailable returns true iff /dev/kvm is openable. Cheap check
// avoids the cost of probing CPU flags; the device file is the
// authoritative liveness signal anyway.
func (c *LinuxCollector) kvmAvailable() bool {
	info, err := os.Stat("/dev/kvm")
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeDevice != 0 || info.Mode().IsRegular()
}

// nestedVirtEnabled reads /sys/module/kvm_{intel,amd}/parameters/nested.
// Either "Y", "1", or "true" → enabled.
func (c *LinuxCollector) nestedVirtEnabled() bool {
	for _, path := range []string{
		"/sys/module/kvm_intel/parameters/nested",
		"/sys/module/kvm_amd/parameters/nested",
	} {
		// #nosec G304 — path comes from a fixed, package-private list.
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(data))
		switch strings.ToLower(v) {
		case "y", "1", "true":
			return true
		}
	}
	return false
}

// sumRunningVMs iterates the agent's VM list and sums vCPUs / memory
// for rows whose Status is StatusRunning. Non-running VMs do not
// consume host resources (the qemu process exists only for the
// running ones, by design of vm.Manager).
func (c *LinuxCollector) sumRunningVMs() (int32, int64) {
	var cores int32
	var mib int64
	for _, v := range c.vms.List() {
		if v == nil {
			continue
		}
		if v.Status != vm.StatusRunning {
			continue
		}
		cores += clampToInt32(v.VCPUs)
		mib += int64(v.MemoryMB)
	}
	return cores, mib
}

// collectVMs translates the agent's VM list into the HeartbeatVMReport
// shape. Status is mapped to the server-side vm_phase enum value the
// receiver accepts.
func (c *LinuxCollector) collectVMs() []VMReport {
	listed := c.vms.List()
	out := make([]VMReport, 0, len(listed))
	for _, v := range listed {
		if v == nil {
			continue
		}
		phase := mapVMStatus(v.Status)
		if phase == "" {
			continue
		}
		out = append(out, VMReport{
			VMUUID: v.ID,
			Phase:  phase,
		})
	}
	return out
}

// mapVMStatus translates the agent's lifecycle Status to the
// server-side vm_phase enum value the heartbeat validator accepts.
// Returning the empty string drops the entry (e.g. for transient
// "deleting" states the CP doesn't model).
func mapVMStatus(s vm.Status) string {
	switch s {
	case vm.StatusPending, vm.StatusCreating:
		return "pending"
	case vm.StatusRunning:
		return "running"
	case vm.StatusStopping, vm.StatusStopped:
		return "stopped"
	case vm.StatusFailed:
		return "error"
	case vm.StatusDeleting:
		return ""
	default:
		return ""
	}
}

// archFromGo maps the Go runtime.GOARCH constant to the heartbeat
// architecture enum. Anything outside the two supported architectures
// surfaces as an empty string — caller has no way to validate against
// the spec, so the receiver returns 400 architecture / 409
// architecture_mismatch and the agent retries every tick.
func archFromGo(goarch string) string {
	switch goarch {
	case "amd64", "arm64":
		return goarch
	default:
		return ""
	}
}

func splitKV(line string) (key, value string, ok bool) {
	i := strings.Index(line, ":")
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}

func clampNonNegative32(v int32) int32 {
	if v < 0 {
		return 0
	}
	return v
}

func clampNonNegative64(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// clampToInt32 narrows an int to int32, saturating on overflow. Used
// when feeding host-reported counts (cores, port numbers, vCPUs) into
// the wire schema, which fixes those fields at int32. The schema
// upper bounds are well below math.MaxInt32 in practice; the clamp
// keeps gosec G115 quiet without burying the conversion intent.
func clampToInt32(v int) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	if v < math.MinInt32 {
		return math.MinInt32
	}
	return int32(v)
}
