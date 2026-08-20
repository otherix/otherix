// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

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

	completed := seedPinnedVM(t, cli, nodeA.ID)
	completedMig := seedActiveMigration(t, s, completed.ID, nodeA.ID, nodeB.ID)
	completedPhase := store.MigrationPhaseCompleted
	if err := s.UpdateMigrationProgress(ctx, completedMig.ID,
		store.MigrationProgressUpdate{Phase: &completedPhase}); err != nil {
		t.Fatalf("UpdateMigrationProgress(completed): %v", err)
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
