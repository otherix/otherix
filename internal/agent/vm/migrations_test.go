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
		UUID: vmID, Name: "ex", VCPUs: 1, MemoryMB: 512,
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
