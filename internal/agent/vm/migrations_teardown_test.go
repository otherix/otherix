// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/migration"
	"github.com/otherix/otherix/internal/agent/qemu"
)

func TestTeardownIncomingTarget_ReapsAndIdempotent(t *testing.T) {
	m := newTestManager(t)
	m.migPorts = migration.NewPortAllocator(49152, 49153) // one pair -> a leak fails the re-reserve
	migID, vmID := uuid.New(), uuid.New()
	ram, nbd, err := m.migPorts.ReservePair()
	if err != nil {
		t.Fatalf("ReservePair: %v", err)
	}
	if _, err := m.AdoptForMigration(AdoptSpec{
		UUID: vmID, Name: "ex", VCPUs: 1, MemoryMB: 512,
		PoolName: m.defaultTestPool(), Architecture: qemu.ArchAMD64,
		InitialStatus: StatusMigratingIncoming,
	}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	m.migrations.Put(&migration.Record{
		MigrationID: migID, VMID: vmID, Role: migration.RoleTarget,
		Mode: migration.ModeLive, Phase: migration.PhaseSetup, Port: ram, NBDPort: nbd,
	})

	m.teardownIncomingTarget(migID, vmID, ram, nbd, migration.PhaseCancelled, "cancelled")

	if _, err := m.Get(vmID); err == nil {
		t.Errorf("vm still present after teardown")
	}
	if _, _, err := m.migPorts.ReservePair(); err != nil {
		t.Errorf("ReservePair after teardown: %v (a port leaked)", err)
	}
	rec, ok := m.migrations.Get(migID)
	if !ok || rec.Phase != migration.PhaseCancelled {
		t.Errorf("record phase = %v ok=%v, want cancelled", rec.Phase, ok)
	}
	if rec.ErrorMessage != "cancelled" {
		t.Errorf("record error = %q, want cancelled", rec.ErrorMessage)
	}
	// Idempotent: a second call must not panic or error.
	m.teardownIncomingTarget(migID, vmID, ram, nbd, migration.PhaseCancelled, "cancelled")
}

// TestTeardownIncomingTarget_NoReleaseWhenAlreadyTerminal is the ABA
// double-release guard: a stale teardownIncomingTarget (cancelLive passes a
// stale non-terminal snapshot) must NOT free a port pair that a later migration
// has already re-reserved. The release is now gated on winning the
// non-terminal->terminal transition, so a call finding the record already
// terminal leaks its (stale) pair rather than freeing the new owner's live port.
func TestTeardownIncomingTarget_NoReleaseWhenAlreadyTerminal(t *testing.T) {
	m := newTestManager(t)
	m.migPorts = migration.NewPortAllocator(49152, 49153) // exactly one pair
	migID, vmID := uuid.New(), uuid.New()

	// M1 reserved the only pair.
	ram, nbd, err := m.migPorts.ReservePair()
	if err != nil {
		t.Fatalf("ReservePair(M1): %v", err)
	}

	// The racing finalizer already WON: it released the pair and stamped the
	// record terminal.
	m.migPorts.ReleasePair(ram, nbd)
	m.migrations.Put(&migration.Record{
		MigrationID: migID, VMID: vmID, Role: migration.RoleTarget,
		Mode: migration.ModeLive, Phase: migration.PhaseFailed, Port: ram, NBDPort: nbd,
	})

	// A second migration M2 grabbed the now-free pair.
	ram2, nbd2, err := m.migPorts.ReservePair()
	if err != nil {
		t.Fatalf("ReservePair(M2): %v", err)
	}
	if ram2 != ram || nbd2 != nbd {
		t.Fatalf("M2 reserved (%d,%d), want the freed pair (%d,%d)", ram2, nbd2, ram, nbd)
	}

	// The STALE teardown runs with M1's old ports. It must NOT release them - the
	// record is already terminal, so it did not win the transition.
	m.teardownIncomingTarget(migID, vmID, ram, nbd, migration.PhaseCancelled, "cancelled")

	// M2 must still own the only pair: a wrongful release would let this succeed.
	if _, _, err := m.migPorts.ReservePair(); err != migration.ErrNoFreePort {
		t.Errorf("ReservePair after stale teardown = %v, want ErrNoFreePort (M2's pair must not be freed)", err)
	}
}

func TestCancelLive_TargetReapsQemuAndPorts(t *testing.T) {
	m := newTestManager(t)
	m.migPorts = migration.NewPortAllocator(49152, 49153)
	migID, vmID := uuid.New(), uuid.New()
	ram, nbd, err := m.migPorts.ReservePair()
	if err != nil {
		t.Fatalf("ReservePair: %v", err)
	}
	if _, err := m.AdoptForMigration(AdoptSpec{
		UUID: vmID, Name: "ex", VCPUs: 1, MemoryMB: 512,
		PoolName: m.defaultTestPool(), Architecture: qemu.ArchAMD64,
		InitialStatus: StatusMigratingIncoming,
	}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	rec := migration.Record{
		MigrationID: migID, VMID: vmID, Role: migration.RoleTarget,
		Mode: migration.ModeLive, Phase: migration.PhaseSetup, Port: ram, NBDPort: nbd,
	}
	m.migrations.Put(&rec)

	view, ok := m.cancelLive(migID, rec)
	if !ok {
		t.Fatal("cancelLive returned ok=false")
	}
	if view.Phase != string(migration.PhaseCancelled) {
		t.Errorf("phase = %q, want cancelled", view.Phase)
	}
	if _, err := m.Get(vmID); err == nil {
		t.Errorf("target vm still present after cancelLive (leaked)")
	}
	if _, _, err := m.migPorts.ReservePair(); err != nil {
		t.Errorf("ReservePair after cancelLive: %v (port leaked)", err)
	}
}

func TestFailIncomingResume_KeepsDiskAndVM(t *testing.T) {
	m := newTestManager(t)
	m.migPorts = migration.NewPortAllocator(49152, 49153)
	migID, vmID := uuid.New(), uuid.New()
	ram, nbd, err := m.migPorts.ReservePair()
	if err != nil {
		t.Fatalf("ReservePair: %v", err)
	}
	v, err := m.AdoptForMigration(AdoptSpec{
		UUID: vmID, Name: "ex", VCPUs: 1, MemoryMB: 512,
		PoolName: m.defaultTestPool(), Architecture: qemu.ArchAMD64,
		InitialStatus: StatusMigratingIncoming,
	})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	diskDir := filepath.Dir(v.DiskPath)
	if err := os.MkdirAll(diskDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(diskDir, "disk.qcow2"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m.migrations.Put(&migration.Record{
		MigrationID: migID, VMID: vmID, Role: migration.RoleTarget,
		Mode: migration.ModeLive, Phase: migration.PhaseActive, Port: ram, NBDPort: nbd,
	})
	taskID := m.tasks.Create(TaskKindVMMigrate, vmID).ID

	m.failIncomingResume(taskID, migID, vmID, "resume: boom")

	// Disk preserved (never destroy a possibly-post-cutover only-copy).
	if _, err := os.Stat(diskDir); err != nil {
		t.Errorf("disk dir removed by failIncomingResume: %v", err)
	}
	// VM kept, marked failed.
	got, err := m.Get(vmID)
	if err != nil {
		t.Fatalf("vm removed by failIncomingResume: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("status = %v, want failed", got.Status)
	}
	// Ports freed.
	if _, _, err := m.migPorts.ReservePair(); err != nil {
		t.Errorf("ReservePair after fail: %v (leak)", err)
	}
}

func TestFailIncomingResume_NoOpWhenRecordAlreadyTerminal(t *testing.T) {
	m := newTestManager(t)
	migID, vmID := uuid.New(), uuid.New()
	// Record already reaped by a racing teardown (cancelled). No VM registered.
	m.migrations.Put(&migration.Record{
		MigrationID: migID, VMID: vmID, Role: migration.RoleTarget,
		Mode: migration.ModeLive, Phase: migration.PhaseCancelled,
	})
	taskID := m.tasks.Create(TaskKindVMMigrate, vmID).ID

	m.failIncomingResume(taskID, migID, vmID, "resume: boom")

	// Record stays cancelled (not flipped to failed); task finalized failed.
	rec, _ := m.migrations.Get(migID)
	if rec.Phase != migration.PhaseCancelled {
		t.Errorf("phase = %v, want cancelled (untouched)", rec.Phase)
	}
	task := m.tasks.Get(taskID)
	if task == nil || task.Status != TaskStatusFailed {
		t.Errorf("task = %v, want status failed", task)
	}
}
