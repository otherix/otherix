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

// TestReleaseIncomingNBD_FreesBothPorts drives the cold-live release edge: a
// TARGET migration record carries a reserved (ram, nbd) pair. releaseIncomingNBD
// must return BOTH ports to the allocator. With a 2-port range, leaking the NBD
// port leaves the pair exhausted and the next ReservePair fails ErrNoFreePort.
func TestReleaseIncomingNBD_FreesBothPorts(t *testing.T) {
	m := newTestManager(t)
	m.migPorts = migration.NewPortAllocator(49152, 49153) // exactly one pair

	ram, nbd, err := m.migPorts.ReservePair()
	if err != nil {
		t.Fatalf("ReservePair: %v", err)
	}
	vmID := uuid.New()
	m.migrations.Put(&migration.Record{
		MigrationID: uuid.New(), VMID: vmID, Role: migration.RoleTarget,
		Mode: migration.ModeLive, Phase: migration.PhaseSetup, Port: ram, NBDPort: nbd,
	})

	m.releaseIncomingNBD(vmID)

	if _, _, err := m.migPorts.ReservePair(); err != nil {
		t.Fatalf("ReservePair after release: %v (a port leaked)", err)
	}
}

// TestRemoveAdoptedVM_RemovesDiskDir confirms removeAdoptedVM removes the
// per-VM destination disk dir (pre-cutover rollback), not just the agent state
// dir. Safe because every caller is strictly pre-cutover.
func TestRemoveAdoptedVM_RemovesDiskDir(t *testing.T) {
	m := newTestManager(t)
	vmID := uuid.New()
	v, err := m.AdoptForMigration(AdoptSpec{
		UUID: vmID, Name: "ex", VCPUs: 1, MemoryMib: 512,
		PoolName: m.defaultTestPool(), Architecture: qemu.ArchAMD64,
	})
	if err != nil {
		t.Fatalf("AdoptForMigration: %v", err)
	}
	diskDir := filepath.Dir(v.DiskPath)
	if err := os.MkdirAll(diskDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(diskDir, "disk.qcow2"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m.removeAdoptedVM(vmID)
	if _, err := os.Stat(diskDir); !os.IsNotExist(err) {
		t.Errorf("disk dir still present after removeAdoptedVM: stat err = %v", err)
	}
}

// TestCancelMigrationOfflineTargetIsIdempotent pins the observable contract the
// control plane now depends on: an offline TARGET record is reaped exactly once,
// however many times the cancel arrives. Both the operator cancel path
// (propagateCancel) and the worker's terminal-outcome reap (reapTargetIncoming)
// call the agent, and nothing serialises them, so a repeated cancel is ordinary
// traffic rather than an edge case.
//
// The second cancel must leave the record's terminal stamp untouched and must
// NOT free a port that a later migration has since taken.
func TestCancelMigrationOfflineTargetIsIdempotent(t *testing.T) {
	m := newTestManager(t)
	migID := uuid.New()
	port, err := m.migPorts.Reserve()
	if err != nil {
		t.Fatalf("Reserve(): %v", err)
	}
	m.Migrations().Put(&migration.Record{
		MigrationID: migID, VMID: uuid.New(), VMName: "demo",
		Role: migration.RoleTarget, Mode: migration.ModeOffline,
		Phase: migration.PhaseSetup, Port: port,
	})

	if _, ok := m.CancelMigration(migID); !ok {
		t.Fatalf("first CancelMigration returned ok=false")
	}
	first, ok := m.Migrations().Get(migID)
	if !ok {
		t.Fatalf("record absent after first cancel")
	}
	if first.Phase != migration.PhaseCancelled {
		t.Fatalf("phase after first cancel = %q, want %q", first.Phase, migration.PhaseCancelled)
	}

	// A later migration takes the freed port. A second cancel of the ALREADY
	// terminal record must not hand this port back to the pool.
	reclaimed, err := m.migPorts.Reserve()
	if err != nil {
		t.Fatalf("Reserve() after cancel: %v", err)
	}
	if reclaimed != port {
		t.Fatalf("reclaimed port = %d, want the freed %d", reclaimed, port)
	}

	if _, ok := m.CancelMigration(migID); !ok {
		t.Fatalf("second CancelMigration returned ok=false")
	}
	second, ok := m.Migrations().Get(migID)
	if !ok {
		t.Fatalf("record absent after second cancel")
	}
	if !second.CompletedAt.Equal(first.CompletedAt) {
		t.Errorf("CompletedAt restamped by the second cancel: %v -> %v", first.CompletedAt, second.CompletedAt)
	}

	// The port the later migration holds must still be reserved: reserving again
	// has to yield a DIFFERENT port.
	next, err := m.migPorts.Reserve()
	if err != nil {
		t.Fatalf("Reserve() after second cancel: %v", err)
	}
	if next == reclaimed {
		t.Errorf("second cancel freed port %d, which a later migration holds", reclaimed)
	}
}
