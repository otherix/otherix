// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// TestMigrationTriedToLandOn covers the read that tells a stale duplicate apart
// from a migration's leftovers on a target. Completed is the only phase that
// means the cutover committed and the pin moved; every other phase leaves the
// target holding state whose fate the migration workers own.
func TestMigrationTriedToLandOn(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	nodeA := nodeParams(uniqueNodeName("mig-land-src"))
	nodeB := nodeParams(uniqueNodeName("mig-land-tgt"))
	if _, err := s.CreateNode(ctx, nodeA); err != nil {
		t.Fatalf("CreateNode(source): %v", err)
	}
	if _, err := s.CreateNode(ctx, nodeB); err != nil {
		t.Fatalf("CreateNode(target): %v", err)
	}
	unrelated := uuid.New()

	// Each case gets its own VM: a VM may have only one active migration, and
	// the phases below are what distinguishes the cases.
	never := seedPinnedVM(t, cli, nodeA.ID)
	active := seedPinnedVM(t, cli, nodeA.ID)
	seedActiveMigration(t, s, active.ID, nodeA.ID, nodeB.ID)

	failed := seedPinnedVM(t, cli, nodeA.ID)
	failedMig := seedActiveMigration(t, s, failed.ID, nodeA.ID, nodeB.ID)
	failedPhase := store.MigrationPhaseFailed
	if err := s.UpdateMigrationProgress(ctx, failedMig.ID,
		store.MigrationProgressUpdate{Phase: &failedPhase}); err != nil {
		t.Fatalf("UpdateMigrationProgress(failed): %v", err)
	}

	// Reached through the real cutover, which is the only thing that can stamp
	// completed: it moves the pin and the phase in one transaction, and that
	// coupling is exactly what makes completed admissible evidence here.
	completed := seedPinnedVM(t, cli, nodeA.ID)
	completedMig := seedActiveMigration(t, s, completed.ID, nodeA.ID, nodeB.ID)
	if err := s.CommitMigrationCutover(ctx, completedMig.ID); err != nil {
		t.Fatalf("CommitMigrationCutover: %v", err)
	}

	tests := []struct {
		name   string
		vmID   uuid.UUID
		nodeID uuid.UUID
		want   bool
	}{
		{name: "no migration at all", vmID: never.ID, nodeID: nodeB.ID, want: false},
		{name: "active migration targets the node", vmID: active.ID, nodeID: nodeB.ID, want: true},
		{name: "active migration, asked about the source", vmID: active.ID, nodeID: nodeA.ID, want: false},
		{name: "failed migration targeted the node", vmID: failed.ID, nodeID: nodeB.ID, want: true},
		{name: "completed migration targeted the node", vmID: completed.ID, nodeID: nodeB.ID, want: false},
		{name: "unrelated node", vmID: failed.ID, nodeID: unrelated, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.MigrationTriedToLandOn(ctx, tt.vmID, tt.nodeID)
			if err != nil {
				t.Fatalf("MigrationTriedToLandOn(%v, %v) = %v, want nil", tt.vmID, tt.nodeID, err)
			}
			if got != tt.want {
				t.Errorf("MigrationTriedToLandOn(%v, %v) = %v, want %v", tt.vmID, tt.nodeID, got, tt.want)
			}
		})
	}
}

// TestRetentionKeepsAnUnresolvedMigrationOfALiveVM pins the lifetime of the
// evidence the re-homed teardown reads. A migration that ended WITHOUT a
// committed cutover is the only durable trace that its target may still be
// holding the guest or the sole destination disk; sweeping it on age would
// silently disarm MigrationTriedToLandOn and let the next heartbeat order that
// copy destroyed, with a retention tick as the only proximate cause.
//
// So while the VM is live such a row outlives its window. Completed rows are
// ordinary history and still expire, and once the VM is gone the incident is
// moot and the row expires too.
func TestRetentionKeepsAnUnresolvedMigrationOfALiveVM(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	node := nodeParams(uniqueNodeName("mig-retain"))
	if _, err := s.CreateNode(ctx, node); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	old := now.Add(-40 * 24 * time.Hour)
	sweep := store.DeleteExpiredMigrationsParams{
		CompletedCutoff: now.Add(-7 * 24 * time.Hour),
		FailedCutoff:    now.Add(-30 * 24 * time.Hour),
		CancelledCutoff: now.Add(-30 * 24 * time.Hour),
	}

	liveVM := seedPinnedVM(t, cli, node.ID)
	keptFailed := seedAgedTargetedMigration(t, cli, liveVM.ID, node.ID, store.MigrationPhaseFailed, old)
	keptCancelled := seedAgedTargetedMigration(t, cli, liveVM.ID, node.ID, store.MigrationPhaseCancelled, old)
	// Same live VM, but a completed migration: the cutover moved the pin, so the
	// row is history and expires normally.
	sweptCompleted := seedAgedTargetedMigration(t, cli, liveVM.ID, node.ID, store.MigrationPhaseCompleted, old)
	// A failed migration whose VM is gone: nothing left to protect.
	sweptOrphan := seedAgedTargetedMigration(t, cli, uuid.New(), node.ID, store.MigrationPhaseFailed, old)

	if _, err := s.DeleteExpiredMigrations(ctx, sweep); err != nil {
		t.Fatalf("DeleteExpiredMigrations: %v", err)
	}

	for _, tc := range []struct {
		name     string
		id       uuid.UUID
		wantGone bool
	}{
		{name: "failed migration of a live vm", id: keptFailed, wantGone: false},
		{name: "cancelled migration of a live vm", id: keptCancelled, wantGone: false},
		{name: "completed migration of a live vm", id: sweptCompleted, wantGone: true},
		{name: "failed migration of a vm that is gone", id: sweptOrphan, wantGone: true},
	} {
		_, err := s.MigrationByID(ctx, tc.id)
		gone := errors.Is(err, store.ErrNotFound)
		if err != nil && !gone {
			t.Fatalf("MigrationByID(%s): %v", tc.name, err)
		}
		if gone != tc.wantGone {
			t.Errorf("%s: swept = %v, want %v", tc.name, gone, tc.wantGone)
		}
	}

	// The guard the retention exists to protect still answers after the sweep.
	landed, err := s.MigrationTriedToLandOn(ctx, liveVM.ID, node.ID)
	if err != nil {
		t.Fatalf("MigrationTriedToLandOn: %v", err)
	}
	if !landed {
		t.Error("MigrationTriedToLandOn after the sweep = false, want true (the kept rows must still answer)")
	}
}

// seedAgedTargetedMigration writes a terminal migration row aged to at, naming
// target as its target node, plus its per-VM history index entry - the shape the
// retention sweep and MigrationTriedToLandOn both read.
func seedAgedTargetedMigration(t *testing.T, cli *etcd.Client, vmID, target uuid.UUID, phase store.MigrationPhase, at time.Time) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	tgt := target
	m := store.Migration{
		ID: id, VmID: vmID, TargetNodeID: &tgt,
		Reason: store.MigrationReasonManual, Phase: phase,
		CompletedAt: &at, CreatedAt: at, UpdatedAt: at,
	}
	if err := cli.PutJSON(ctx, etcd.Key("migrations", id.String()), m); err != nil {
		t.Fatalf("seed migration row: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "migrations", "vm", vmID.String(), id.String()),
		[]byte(id.String())); err != nil {
		t.Fatalf("seed migration vm index: %v", err)
	}
	return id
}
