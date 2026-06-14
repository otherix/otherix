// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// migrationJobArgsStub is a minimal queue.JobArgs the migration enqueue path can
// marshal onto the backing job.
type migrationJobArgsStub struct {
	TaskID      uuid.UUID
	MigrationID uuid.UUID
}

func (migrationJobArgsStub) Kind() string { return "vm.migrate" }

func migrationParams(vmID, sourceNode, targetNode uuid.UUID) store.CreateMigrationParams {
	src := sourceNode
	tgt := targetNode
	taskID := uuid.New()
	return store.CreateMigrationParams{
		ID:           uuid.New(),
		VmID:         vmID,
		SourceNodeID: &src,
		TargetNodeID: &tgt,
		Reason:       store.MigrationReasonManual,
		Live:         true,
		Task: store.CreateTaskParams{
			ID:           taskID,
			Type:         "vm.migrate",
			Status:       store.TaskStatusPending,
			ResourceType: "migration",
			MaxAttempts:  3,
		},
	}
}

func TestCreateMigration_WritesRowAndGuards(t *testing.T) {
	s, cl := startStore(t)
	ctx := context.Background()

	sourceNode := nodeParams(uniqueNodeName("src"))
	targetNode := nodeParams(uniqueNodeName("tgt"))
	if _, err := s.CreateNode(ctx, sourceNode); err != nil {
		t.Fatalf("CreateNode(source): %v", err)
	}
	if _, err := s.CreateNode(ctx, targetNode); err != nil {
		t.Fatalf("CreateNode(target): %v", err)
	}

	vmID := uuid.New()
	p := migrationParams(vmID, sourceNode.ID, targetNode.ID)
	args := migrationJobArgsStub{TaskID: p.Task.ID, MigrationID: p.ID}

	m, err := s.CreateMigration(ctx, p, args)
	if err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}
	if m.ID != p.ID {
		t.Errorf("CreateMigration ID = %v, want %v", m.ID, p.ID)
	}
	if m.Phase != store.MigrationPhasePending {
		t.Errorf("CreateMigration Phase = %q, want %q", m.Phase, store.MigrationPhasePending)
	}
	if m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() {
		t.Errorf("CreateMigration timestamps not stamped: created=%v updated=%v", m.CreatedAt, m.UpdatedAt)
	}

	// Primary row is readable at /otherix/migrations/<id>.
	var got store.Migration
	found, err := cl.GetJSON(ctx, etcd.Key("migrations", p.ID.String()), &got)
	if err != nil {
		t.Fatalf("GetJSON(migration primary): %v", err)
	}
	if !found {
		t.Fatalf("migration primary row missing at %s", etcd.Key("migrations", p.ID.String()))
	}
	if got.VmID != vmID || got.Phase != store.MigrationPhasePending {
		t.Errorf("stored migration = {vm:%v phase:%q}, want {vm:%v phase:pending}", got.VmID, got.Phase, vmID)
	}

	// Per-node index entry exists under the prefix the existing reader ranges,
	// with the migration id as the value (activeMigrationsOnNode parses the value).
	srcIdx, err := cl.Range(ctx, etcd.Key("index", "migrations", "node", sourceNode.ID.String())+"/")
	if err != nil {
		t.Fatalf("Range(source node index): %v", err)
	}
	if len(srcIdx) != 1 || string(srcIdx[0].Value) != p.ID.String() {
		t.Errorf("source node index = %+v, want one entry valued %s", srcIdx, p.ID)
	}
	tgtIdx, err := cl.Range(ctx, etcd.Key("index", "migrations", "node", targetNode.ID.String())+"/")
	if err != nil {
		t.Fatalf("Range(target node index): %v", err)
	}
	if len(tgtIdx) != 1 || string(tgtIdx[0].Value) != p.ID.String() {
		t.Errorf("target node index = %+v, want one entry valued %s", tgtIdx, p.ID)
	}

	// VM index entry exists.
	vmIdx, err := cl.Range(ctx, etcd.Key("index", "migrations", "vm", vmID.String())+"/")
	if err != nil {
		t.Fatalf("Range(vm index): %v", err)
	}
	if len(vmIdx) != 1 || string(vmIdx[0].Value) != p.ID.String() {
		t.Errorf("vm index = %+v, want one entry valued %s", vmIdx, p.ID)
	}

	// Backing task row was written.
	if _, err := s.TaskByID(ctx, p.Task.ID); err != nil {
		t.Errorf("TaskByID(backing task) = %v, want nil", err)
	}

	// A second active migration for the same VM is rejected by the guard.
	p2 := migrationParams(vmID, sourceNode.ID, targetNode.ID)
	args2 := migrationJobArgsStub{TaskID: p2.Task.ID, MigrationID: p2.ID}
	if _, err := s.CreateMigration(ctx, p2, args2); !errors.Is(err, store.ErrMigrationActiveExists) {
		t.Errorf("second CreateMigration = %v, want store.ErrMigrationActiveExists", err)
	}
}
