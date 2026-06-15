// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/migration"
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

