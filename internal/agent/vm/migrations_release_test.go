// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/migration"
)

// TestReleaseIncomingNBDFreesPortAndDropsRecord drives the post-cutover
// release path: a TARGET migration record (with a reserved port, NBDPid=0 so
// StopNBD no-ops) is dropped from the store and its port returned to the
// allocator. The port-release assertion exhausts the whole range afterwards
// and confirms the freed port is reservable again.
func TestReleaseIncomingNBDFreesPortAndDropsRecord(t *testing.T) {
	m := newTestManager(t)

	// Reserve one ingress port the way StartIncoming would, then record it on
	// a target migration for vmID. NBDPid=0 makes StopNBD a no-op.
	port, err := m.migPorts.Reserve()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	vmID := uuid.New()
	migID := uuid.New()
	m.Migrations().Put(&migration.Record{
		MigrationID: migID, VMID: vmID, Role: migration.RoleTarget,
		Mode: migration.ModeOffline, Phase: migration.PhaseCompleted,
		Port: port, NBDPid: 0,
	})

	m.releaseIncomingNBD(vmID)

	// Record dropped.
	if _, ok := m.Migrations().Get(migID); ok {
		t.Errorf("Get(%s) after releaseIncomingNBD = found, want absent", migID)
	}

	// Port returned to the pool: drain the full [start, end] range and confirm
	// the reserved port appears among the free ports. With the record's port
	// released, every port in the range is free, so the drain yields the full
	// range count and includes the freed port.
	freed := false
	count := 0
	for {
		p, err := m.migPorts.Reserve()
		if err != nil {
			break
		}
		count++
		if p == port {
			freed = true
		}
	}
	rangeSize := 49251 - 49152 + 1
	if count != rangeSize {
		t.Errorf("drained %d ports, want full range of %d (a port leaked or was double-counted)", count, rangeSize)
	}
	if !freed {
		t.Errorf("freed port %d not reservable after releaseIncomingNBD", port)
	}
}
