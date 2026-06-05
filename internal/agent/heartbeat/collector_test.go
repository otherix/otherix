// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/config"
)

type stubLister struct{ vms []*vm.VM }

func (s stubLister) List() []*vm.VM { return s.vms }

// TestLinuxCollector_ReadsProcCpuinfoAndMeminfo lays a synthetic
// /proc tree in a tempdir and confirms the collector parses the
// canonical Linux file formats correctly. We do not exercise the
// real /proc here — the syscall integration is covered by the
// runtime test below, and the parsers are the brittle parts worth
// pinning.
func TestLinuxCollector_ReadsProcCpuinfoAndMeminfo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(syntheticCPUInfo), 0o644); err != nil {
		t.Fatalf("write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(syntheticMemInfo), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}

	c := &LinuxCollector{
		procPath: dir,
		vms:      stubLister{},
	}
	model, flags, cores := c.readCPUInfo()
	if model != "AMD EPYC 9554" {
		t.Errorf("model = %q, want %q", model, "AMD EPYC 9554")
	}
	if cores != 4 {
		t.Errorf("cores = %d, want 4", cores)
	}
	if len(flags) == 0 || flags[0] != "fpu" {
		t.Errorf("flags[0] = %q, want fpu", flags[0])
	}

	mib := c.readMemTotalMib()
	if mib != 16384 {
		t.Errorf("memory_total_mib = %d, want 16384", mib)
	}
}

// TestLinuxCollector_FreeResourceSubtraction confirms the
// running-VM allocation arithmetic. Non-running VMs are excluded;
// negative values clamp to zero.
func TestLinuxCollector_FreeResourceSubtraction(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(syntheticCPUInfo), 0o644); err != nil {
		t.Fatalf("write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(syntheticMemInfo), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	vms := []*vm.VM{
		{ID: uuid.New(), Status: vm.StatusRunning, VCPUs: 2, MemoryMB: 2048},
		{ID: uuid.New(), Status: vm.StatusRunning, VCPUs: 1, MemoryMB: 1024},
		{ID: uuid.New(), Status: vm.StatusStopped, VCPUs: 99, MemoryMB: 99999}, // ignored
	}
	c := &LinuxCollector{
		procPath:     dir,
		vms:          stubLister{vms: vms},
		agentVersion: "test",
		architecture: "amd64",
	}

	rep, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got, want := rep.Resources.CPUCoresAvailable, int32(4-3); got != want {
		t.Errorf("cpu_cores_available = %d, want %d", got, want)
	}
	if got, want := rep.Resources.MemoryAvailableMib, int64(16384-3072); got != want {
		t.Errorf("memory_available_mib = %d, want %d", got, want)
	}
	if got, want := len(rep.VMs), 3; got != want {
		// Stopped VM still surfaces in the heartbeat (with phase=stopped) —
		// CP-side reconciler reads inventory from full snapshots, not
		// only running rows.
		t.Errorf("vms count = %d, want %d", got, want)
	}
}

type stubPoolReporter struct{ reports []PoolReport }

func (s stubPoolReporter) PoolReports() []PoolReport { return s.reports }

type fakePoolImageLister struct{ images map[string][]PoolImageReport }

func (f fakePoolImageLister) PoolImages(pool string) []PoolImageReport { return f.images[pool] }

// TestCollectPoolImages confirms the collector folds the per-pool
// image inventory (from the PoolImageLister seam) into the matching
// PoolReport.Images for the heartbeat.
func TestCollectPoolImages(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(syntheticCPUInfo), 0o644); err != nil {
		t.Fatalf("write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(syntheticMemInfo), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	img := PoolImageReport{
		Basename:   "noble.qcow2",
		SHA256:     "abc123",
		SizeBytes:  1024,
		Format:     "qcow2",
		ImportedAt: "2026-06-05T00:00:00Z",
	}
	c := &LinuxCollector{
		procPath:     dir,
		vms:          stubLister{},
		agentVersion: "test",
		architecture: "amd64",
		pools:        stubPoolReporter{reports: []PoolReport{{Name: "default", ReconciliationStatus: "ready"}}},
		poolImages:   fakePoolImageLister{images: map[string][]PoolImageReport{"default": {img}}},
	}

	rep, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := len(rep.Pools); got != 1 {
		t.Fatalf("pools count = %d, want 1", got)
	}
	if diff := cmp.Diff([]PoolImageReport{img}, rep.Pools[0].Images); diff != "" {
		t.Errorf("Pools[0].Images mismatch (-want +got):\n%s", diff)
	}
}

// TestMapVMStatus pins the agent→server phase mapping. The CP-side
// validator rejects anything outside the supported VmPhase enum, so
// drift here surfaces as a run-time 400.
func TestMapVMStatus(t *testing.T) {
	cases := map[vm.Status]string{
		vm.StatusPending:  "pending",
		vm.StatusCreating: "pending",
		vm.StatusRunning:  "running",
		vm.StatusStopping: "stopped",
		vm.StatusStopped:  "stopped",
		vm.StatusFailed:   "error",
		vm.StatusDeleting: "",
	}
	for in, want := range cases {
		if got := mapVMStatus(in); got != want {
			t.Errorf("mapVMStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestArchFromGo pins GOARCH translation.
func TestArchFromGo(t *testing.T) {
	cases := map[string]string{
		"amd64": "amd64",
		"arm64": "arm64",
		"386":   "",
		"":      "",
	}
	for in, want := range cases {
		if got := archFromGo(in); got != want {
			t.Errorf("archFromGo(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestClampNonNegative confirms underflow handling — sum of
// running VMs may exceed total reported cores/memory if the
// host's accounting drifts. Negative values surface as 0 rather
// than wrap-around to a huge positive number.
func TestClampNonNegative(t *testing.T) {
	if got := clampNonNegative32(-5); got != 0 {
		t.Errorf("clampNonNegative32(-5) = %d, want 0", got)
	}
	if got := clampNonNegative32(7); got != 7 {
		t.Errorf("clampNonNegative32(7) = %d, want 7", got)
	}
	if got := clampNonNegative64(-1); got != 0 {
		t.Errorf("clampNonNegative64(-1) = %d, want 0", got)
	}
}

// TestNewLinux_RejectsMissingVMs ensures the constructor surfaces
// a usable error rather than panicking when wiring is incomplete.
func TestNewLinux_RejectsMissingVMs(t *testing.T) {
	_, err := NewLinux(CollectorDeps{Migration: config.MigrationConfig{}})
	if err == nil {
		t.Fatal("expected error when VMs lister is nil")
	}
}

const syntheticCPUInfo = `processor	: 0
vendor_id	: AuthenticAMD
model name	: AMD EPYC 9554
flags		: fpu vme de pse tsc msr

processor	: 1
vendor_id	: AuthenticAMD
model name	: AMD EPYC 9554
flags		: fpu vme de pse tsc msr

processor	: 2
vendor_id	: AuthenticAMD
model name	: AMD EPYC 9554
flags		: fpu vme de pse tsc msr

processor	: 3
vendor_id	: AuthenticAMD
model name	: AMD EPYC 9554
flags		: fpu vme de pse tsc msr
`

const syntheticMemInfo = `MemTotal:       16777216 kB
MemFree:         1234567 kB
MemAvailable:    9876543 kB
`
