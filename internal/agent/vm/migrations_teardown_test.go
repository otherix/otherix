// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
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
