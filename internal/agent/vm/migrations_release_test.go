// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/migration"
	"github.com/otherix/otherix/internal/agent/qemu"
)

// TestReleaseIncomingFreesPortAndDropsRecord drives the post-cutover release
// path: a TARGET migration record with a reserved port and a server is dropped
// from the store, its server stopped and its port returned to the allocator.
// The port-release assertion exhausts the whole range afterwards and confirms
// the freed port is reservable again.
//
// The record is non-terminal because that is what the real path carries here:
// nothing advances a target-side offline record after StartIncoming publishes it,
// so it is still at phase=setup when the post-cutover start arrives. A terminal
// record means some other finalizer already released these ports, and
// releaseIncoming must NOT release them a second time - see
// TestReleaseIncomingNeverTouchesATerminalRecord.
func TestReleaseIncomingFreesPortAndDropsRecord(t *testing.T) {
	m := newTestManager(t)
	var stopped []int
	m.migStopNBD = func(srv *qemu.NBDServer, _ time.Duration) error {
		stopped = append(stopped, srv.Pid)
		return nil
	}

	// Reserve one ingress port the way StartIncoming would, then record it on
	// a target migration for vmID.
	port, err := m.migPorts.Reserve()
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	vmID := uuid.New()
	migID := uuid.New()
	m.Migrations().Put(&migration.Record{
		MigrationID: migID, VMID: vmID, Role: migration.RoleTarget,
		Mode: migration.ModeOffline, Phase: migration.PhaseSetup,
		Port: port, NBD: &qemu.NBDServer{Pid: 4242},
	})

	m.releaseIncoming(vmID, false)

	if len(stopped) != 1 || stopped[0] != 4242 {
		t.Errorf("stopped %v, want exactly [4242]", stopped)
	}
	// Record dropped.
	if _, ok := m.Migrations().Get(migID); ok {
		t.Errorf("Get(%s) after releaseIncoming = found, want absent", migID)
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
		t.Errorf("freed port %d not reservable after releaseIncoming", port)
	}
}
