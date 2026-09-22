// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/reconciler"
	"github.com/otherix/otherix/internal/agent/vm"
)

// These drive the real Manager, migration store and VM reconciler together: the
// unit tests on either side use a fake of the other, and the property that
// matters - which responses move the reconciler to act on a migrated VM - lives
// in the seam between them.

func startColdIncoming(t *testing.T, m *vm.Manager, vmID uuid.UUID) uuid.UUID {
	t.Helper()
	migID := uuid.New()
	if _, err := m.StartIncoming(context.Background(), vm.IncomingSpec{
		MigrationID: migID, VMUUID: vmID, VMName: "cold", VCPUs: 1, MemoryMib: 512,
		PoolName: m.DefaultPoolForSeamTest(), Architecture: "amd64", Mode: "offline",
		DiskSizeBytes: 1 << 30, SourceIdentity: "CN=node-src", BindHost: "10.0.0.2",
	}); err != nil {
		t.Fatalf("StartIncoming: %v", err)
	}
	return migID
}

func runReconciler(t *testing.T, m *vm.Manager) *reconciler.VMs {
	t.Helper()
	r, err := reconciler.NewVMs(m, slog.New(slog.NewTextHandler(io.Discard, nil)), 20*time.Millisecond)
	if err != nil {
		t.Fatalf("NewVMs: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return r
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mustStatus(t *testing.T, m *vm.Manager, id uuid.UUID) vm.Status {
	t.Helper()
	v, err := m.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return v.Status
}

func TestAStaleDeclarationNeitherStartsNorReleasesAMigratedVM(t *testing.T) {
	m, stops := vm.NewManagerForSeamTest(t)
	vmID := uuid.New()
	requested := time.Now()
	startColdIncoming(t, m, vmID)
	r := runReconciler(t, m)

	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredVMs:   []heartbeat.DeclaredVM{{Name: "cold", DesiredPhase: "running", Generation: 1}},
		RequestSentAt: requested,
	})
	time.Sleep(300 * time.Millisecond) // many ticks

	if got := stops.Pids(); len(got) != 0 {
		t.Errorf("stopped %v, want nothing on a response older than the arrival", got)
	}
	if !m.HasActiveMigration(vmID) {
		t.Errorf("HasActiveMigration = false, want the in-flight record kept")
	}
	if got := mustStatus(t, m, vmID); got != vm.StatusStopped {
		t.Errorf("status = %s, want stopped (a start would boot a half-written disk)", got)
	}
}

func TestAStaleDeclarationStartsNothingAfterTheMigrationIsCancelled(t *testing.T) {
	m, _ := vm.NewManagerForSeamTest(t)
	vmID := uuid.New()
	requested := time.Now()
	migID := startColdIncoming(t, m, vmID)
	m.CancelMigration(migID) // the record goes terminal; the adopted copy stays
	r := runReconciler(t, m)

	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredVMs:   []heartbeat.DeclaredVM{{Name: "cold", DesiredPhase: "running", Generation: 1}},
		RequestSentAt: requested,
	})
	time.Sleep(300 * time.Millisecond)

	if got := mustStatus(t, m, vmID); got != vm.StatusStopped {
		t.Errorf("status = %s, want stopped: the copy of a cancelled migration was started from a stale declaration", got)
	}
}

func TestAStaleTombstoneDoesNotDestroyAVMThatMigratedBack(t *testing.T) {
	m, _ := vm.NewManagerForSeamTest(t)
	vmID := uuid.New()
	requested := time.Now()
	migID := startColdIncoming(t, m, vmID)
	m.CancelMigration(migID) // HasActiveMigration false: only the fence stands in the way
	r := runReconciler(t, m)

	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		VMTombstones:  []heartbeat.VMTombstone{{VMID: vmID, VMName: "cold"}},
		RequestSentAt: requested,
	})
	time.Sleep(300 * time.Millisecond)

	if _, err := m.Get(vmID); err != nil {
		t.Errorf("Get after a stale tombstone = %v, want the VM kept", err)
	}
}

func TestCompletedColdMigrationIsReleasedAndThenTornDown(t *testing.T) {
	m, stops := vm.NewManagerForSeamTest(t)
	vmID := uuid.New()
	startColdIncoming(t, m, vmID)
	r := runReconciler(t, m)

	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredVMs:   []heartbeat.DeclaredVM{{Name: "cold", DesiredPhase: "stopped", Generation: 1}},
		RequestSentAt: time.Now(),
	})
	eventually(t, "the incoming server to be stopped", func() bool { return len(stops.Pids()) == 1 })
	eventually(t, "the record to be released", func() bool { return !m.HasActiveMigration(vmID) })

	// With the record gone the teardown gate opens: a fresh tombstone removes the copy.
	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		VMTombstones:  []heartbeat.VMTombstone{{VMID: vmID, VMName: "cold"}},
		RequestSentAt: time.Now(),
	})
	eventually(t, "the tombstoned VM to be removed", func() bool {
		_, err := m.Get(vmID)
		return err != nil
	})
	if got := stops.Pids(); len(got) != 1 {
		t.Errorf("stopped %v, want exactly one stop (the delete finds nothing left to release)", got)
	}
}
