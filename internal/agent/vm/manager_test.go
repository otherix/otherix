// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/qemu"
	"github.com/otherix/otherix/internal/agent/state"
	"github.com/otherix/otherix/internal/config"
)

func newTestConfig(t *testing.T) (*config.AgentConfig, string, string) {
	t.Helper()
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "vms")
	poolRoot := filepath.Join(tmp, "pools", "default")
	poolName := "default"
	cfg := &config.AgentConfig{
		StatePath: stateDir,
		QEMU: config.QEMUConfig{
			AArch64FirmwarePath: "/usr/share/AAVMF/AAVMF_CODE.fd",
		},
	}
	return cfg, poolRoot, poolName
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestManager_New_ValidatesStatePath(t *testing.T) {
	cases := []struct {
		name    string
		mut     func(*config.AgentConfig)
		wantErr bool
	}{
		{"empty state path", func(c *config.AgentConfig) { c.StatePath = "" }, true},
		{"valid", func(*config.AgentConfig) {}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _ := newTestConfig(t)
			tc.mut(cfg)
			_, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
			if tc.wantErr && err == nil {
				t.Fatalf("New(%s) = nil, want error", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("New(%s) = %v, want nil", tc.name, err)
			}
		})
	}
}

func TestManager_AddPool_Validates(t *testing.T) {
	cases := []struct {
		name    string
		pool    string
		root    func(t *testing.T) string
		wantErr bool
	}{
		{"empty name", "", func(*testing.T) string { return "/tmp" }, true},
		{"empty root", "p", func(*testing.T) string { return "" }, true},
		{"relative root", "p", func(*testing.T) string { return "relative" }, true},
		{"valid", "p", func(t *testing.T) string { return t.TempDir() }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, _ := newTestConfig(t)
			m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			err = m.AddPool(tc.pool, tc.root(t))
			if tc.wantErr && err == nil {
				t.Fatalf("AddPool(%s) = nil, want error", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("AddPool(%s) = %v, want nil", tc.name, err)
			}
		})
	}
}

func TestManager_Create_ValidationErrors(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	validChecksum := "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	cases := []struct {
		name string
		spec CreateSpec
	}{
		{"empty name", CreateSpec{Name: "", VCPUs: 2, MemoryMB: 1024, PoolName: poolName, TemplateChecksum: validChecksum}},
		{"low vcpus", CreateSpec{Name: "x", VCPUs: 0, MemoryMB: 1024, PoolName: poolName, TemplateChecksum: validChecksum}},
		{"high vcpus", CreateSpec{Name: "x", VCPUs: 200, MemoryMB: 1024, PoolName: poolName, TemplateChecksum: validChecksum}},
		{"low memory", CreateSpec{Name: "x", VCPUs: 2, MemoryMB: 64, PoolName: poolName, TemplateChecksum: validChecksum}},
		{"empty checksum", CreateSpec{Name: "x", VCPUs: 2, MemoryMB: 1024, PoolName: poolName, TemplateChecksum: ""}},
		{"non-hex checksum", CreateSpec{Name: "x", VCPUs: 2, MemoryMB: 1024, PoolName: poolName, TemplateChecksum: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}},
		{"empty pool", CreateSpec{Name: "x", VCPUs: 2, MemoryMB: 1024, PoolName: "", TemplateChecksum: validChecksum}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.Create(t.Context(), tc.spec)
			if err == nil {
				t.Fatalf("Create(%s) = nil, want error", tc.name)
			}
			if !errors.Is(err, ErrInvalidSpec) {
				t.Errorf("Create(%s) error = %v, want ErrInvalidSpec", tc.name, err)
			}
		})
	}
}

func TestManager_Create_UnknownPool(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}
	validChecksum := "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	_, err = m.Create(t.Context(), CreateSpec{
		Name:             "x",
		VCPUs:            2,
		MemoryMB:         1024,
		PoolName:         "not-the-configured-pool",
		TemplateChecksum: validChecksum,
	})
	if !errors.Is(err, ErrPoolUnknown) {
		t.Errorf("err = %v, want ErrPoolUnknown", err)
	}
}

func TestManager_Get_NotFound(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := m.Get(uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestManager_List_Empty(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := m.List(); len(got) != 0 {
		t.Errorf("List() = %d entries, want 0", len(got))
	}
}

func TestManager_InFlightGuard_AcquireReleaseAndQuery(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if m.HasInFlight("foo") {
		t.Errorf("HasInFlight(foo) = true before any acquire")
	}
	release, ok := m.inFlightAcquire("foo")
	if !ok {
		t.Fatalf("first acquire on foo failed")
	}
	if !m.HasInFlight("foo") {
		t.Errorf("HasInFlight(foo) = false after acquire")
	}
	if _, ok := m.inFlightAcquire("foo"); ok {
		t.Errorf("second acquire on foo succeeded (must reject)")
	}
	if _, ok := m.inFlightAcquire("bar"); !ok {
		t.Errorf("acquire on independent name bar failed")
	}
	release()
	if m.HasInFlight("foo") {
		t.Errorf("HasInFlight(foo) = true after release")
	}
	if _, ok := m.inFlightAcquire("foo"); !ok {
		t.Errorf("re-acquire on foo after release failed")
	}
}

// TestManager_FailTaskOnly_DoesNotMutateVMStatus pins the contract of
// failTaskOnly: it marks the task as failed but leaves the VM's
// persisted status unchanged. Used by stop / reboot timeout paths
// where the VM is still running (qemu refused to honour ACPI); the
// task surfaces the failure but the VM itself is not in StatusFailed.
func TestManager_FailTaskOnly_DoesNotMutateVMStatus(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	vmID := uuid.New()
	m.mu.Lock()
	m.vms[vmID] = &VM{
		ID:     vmID,
		Name:   "test-vm",
		Status: StatusRunning,
	}
	m.mu.Unlock()

	task := m.tasks.Create(TaskKindVMStop, vmID)
	m.failTaskOnly(task.ID, "stop_timeout", "guest ignored ACPI")

	got := m.tasks.Get(task.ID)
	if got == nil {
		t.Fatal("task not found after failTaskOnly")
	}
	if got.Status != TaskStatusFailed {
		t.Errorf("task.Status = %q, want %q", got.Status, TaskStatusFailed)
	}
	if got.Error == nil {
		t.Fatal("task.Error = nil, want populated")
	}
	if got.Error.Code != "stop_timeout" || got.Error.Message != "guest ignored ACPI" {
		t.Errorf("task.Error = {%q, %q}, want {%q, %q}",
			got.Error.Code, got.Error.Message, "stop_timeout", "guest ignored ACPI")
	}

	v, err := m.snapshotVM(vmID)
	if err != nil {
		t.Fatalf("snapshotVM: %v", err)
	}
	if v.Status != StatusRunning {
		t.Errorf("VM status mutated by failTaskOnly: got %q, want %q (unchanged)",
			v.Status, StatusRunning)
	}
}

// TestManager_FailTask_MutatesVMStatusToFailed locks the existing
// semantics of failTask: VM transitions to StatusFailed AND task is
// recorded as failed. Reserved for paths where the VM legitimately
// entered a failed state (create / start spawn failures, stuck-qemu
// poweroff escalation).
func TestManager_FailTask_MutatesVMStatusToFailed(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	vmID := uuid.New()
	m.mu.Lock()
	m.vms[vmID] = &VM{
		ID:     vmID,
		Name:   "test-vm",
		Status: StatusCreating,
	}
	m.mu.Unlock()

	task := m.tasks.Create(TaskKindVMCreate, vmID)
	m.failTask(task.ID, vmID, "qemu_spawn_failed", "boom")

	got := m.tasks.Get(task.ID)
	if got == nil {
		t.Fatal("task not found after failTask")
	}
	if got.Status != TaskStatusFailed {
		t.Errorf("task.Status = %q, want %q", got.Status, TaskStatusFailed)
	}

	v, err := m.snapshotVM(vmID)
	if err != nil {
		t.Fatalf("snapshotVM: %v", err)
	}
	if v.Status != StatusFailed {
		t.Errorf("VM status = %q, want %q (failTask must mark VM failed)",
			v.Status, StatusFailed)
	}
}

// TestManager_New_SweepsOrphanTaps locks the startup orphan-tap sweep:
// taps the host reports that do NOT belong to any replayed VM are
// reclaimed; taps that DO belong to a replayed VM are left alone.
func TestManager_New_SweepsOrphanTaps(t *testing.T) {
	cfg, _, _ := newTestConfig(t)

	// A replayed VM whose single NIC implies a kept tap.
	vmID := uuid.New()
	nic := sampleNIC()
	keepTap := nic.TapName()
	meta := &state.VMMeta{
		VMID:         vmID,
		Name:         "kept-vm",
		VCPUs:        2,
		MemoryMB:     1024,
		PoolName:     "default",
		Architecture: string(qemu.HostArch()),
		Status:       string(StatusStopped),
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
		NICs:         nicsToMeta([]netfabric.NIC{nic}),
	}
	if err := state.WriteMeta(filepath.Join(cfg.StatePath, vmID.String()), meta); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}

	orphanTap := "otdeadbeef0000"
	fake := &netfabric.FakeFabric{ListTapsResult: []string{keepTap, orphanTap}}

	if _, err := New(cfg, fake, discardLogger()); err != nil {
		t.Fatalf("New: %v", err)
	}

	if fake.ListTapsCalls != 1 {
		t.Errorf("ListTapsCalls = %d, want 1", fake.ListTapsCalls)
	}
	want := []string{orphanTap}
	if diff := cmp.Diff(want, fake.DeleteTapCalls); diff != "" {
		t.Errorf("DeleteTapCalls mismatch (-want +got):\n%s", diff)
	}
}

// TestManager_New_SweepsOrphanTaps_NoVMs covers the VM-less manager: a
// host tap with no owning VM is reclaimed.
func TestManager_New_SweepsOrphanTaps_NoVMs(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	orphanTap := "otcccccccc0000"
	fake := &netfabric.FakeFabric{ListTapsResult: []string{orphanTap}}

	if _, err := New(cfg, fake, discardLogger()); err != nil {
		t.Fatalf("New: %v", err)
	}

	want := []string{orphanTap}
	if diff := cmp.Diff(want, fake.DeleteTapCalls); diff != "" {
		t.Errorf("DeleteTapCalls mismatch (-want +got):\n%s", diff)
	}
}

// TestManager_New_OrphanSweep_ToleratesListTapsError ensures a ListTaps
// failure skips the sweep without failing manager startup.
func TestManager_New_OrphanSweep_ToleratesListTapsError(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	fake := &netfabric.FakeFabric{Errs: map[string]error{"ListTaps": errors.New("boom")}}

	m, err := New(cfg, fake, discardLogger())
	if err != nil {
		t.Fatalf("New must not fail on ListTaps error: %v", err)
	}
	if m == nil {
		t.Fatal("New returned nil manager")
	}
	if len(fake.DeleteTapCalls) != 0 {
		t.Errorf("DeleteTapCalls = %v, want none (sweep skipped on ListTaps error)", fake.DeleteTapCalls)
	}
}

func TestManager_InFlightGuard_EmptyName_IsNoOp(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, &netfabric.FakeFabric{}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	release, ok := m.inFlightAcquire("")
	if !ok {
		t.Errorf("empty name acquire returned ok=false (must be no-op true)")
	}
	if m.HasInFlight("") {
		t.Errorf("HasInFlight(\"\") = true (must be no-op false)")
	}
	release()
}
