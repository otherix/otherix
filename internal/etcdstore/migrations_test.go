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

func TestMigrationByID_NotFound(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	if _, err := s.MigrationByID(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("MigrationByID(unknown) = %v, want store.ErrNotFound", err)
	}
}

func TestMigrationByID_ReturnsCreated(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	sourceNode := nodeParams(uniqueNodeName("src"))
	targetNode := nodeParams(uniqueNodeName("tgt"))
	if _, err := s.CreateNode(ctx, sourceNode); err != nil {
		t.Fatalf("CreateNode(source): %v", err)
	}
	if _, err := s.CreateNode(ctx, targetNode); err != nil {
		t.Fatalf("CreateNode(target): %v", err)
	}

	p := migrationParams(uuid.New(), sourceNode.ID, targetNode.ID)
	args := migrationJobArgsStub{TaskID: p.Task.ID, MigrationID: p.ID}
	if _, err := s.CreateMigration(ctx, p, args); err != nil {
		t.Fatalf("CreateMigration: %v", err)
	}

	got, err := s.MigrationByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("MigrationByID(%v) = %v, want nil", p.ID, err)
	}
	if got.ID != p.ID || got.VmID != p.VmID || got.Phase != store.MigrationPhasePending {
		t.Errorf("MigrationByID = {id:%v vm:%v phase:%q}, want {id:%v vm:%v phase:pending}", got.ID, got.VmID, got.Phase, p.ID, p.VmID)
	}
}

func TestListMigrations_VMFilter(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	src := nodeParams(uniqueNodeName("src"))
	tgt := nodeParams(uniqueNodeName("tgt"))
	if _, err := s.CreateNode(ctx, src); err != nil {
		t.Fatalf("CreateNode(source): %v", err)
	}
	if _, err := s.CreateNode(ctx, tgt); err != nil {
		t.Fatalf("CreateNode(target): %v", err)
	}

	vmA := uuid.New()
	vmB := uuid.New()
	mA := migrationParams(vmA, src.ID, tgt.ID)
	mB := migrationParams(vmB, src.ID, tgt.ID)
	if _, err := s.CreateMigration(ctx, mA, migrationJobArgsStub{TaskID: mA.Task.ID, MigrationID: mA.ID}); err != nil {
		t.Fatalf("CreateMigration(A): %v", err)
	}
	if _, err := s.CreateMigration(ctx, mB, migrationJobArgsStub{TaskID: mB.Task.ID, MigrationID: mB.ID}); err != nil {
		t.Fatalf("CreateMigration(B): %v", err)
	}

	got, err := s.ListMigrations(ctx, store.ListMigrationsParams{LimitCount: 50, VmID: &vmA})
	if err != nil {
		t.Fatalf("ListMigrations(VmID=A): %v", err)
	}
	if len(got) != 1 || got[0].ID != mA.ID {
		t.Errorf("ListMigrations(VmID=A) = %+v, want one entry %v", got, mA.ID)
	}
}

func TestListMigrations_NodeFilterMatchesSourceOrTarget(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	a := nodeParams(uniqueNodeName("a"))
	b := nodeParams(uniqueNodeName("b"))
	c := nodeParams(uniqueNodeName("c"))
	for _, n := range []store.CreateNodeParams{a, b, c} {
		if _, err := s.CreateNode(ctx, n); err != nil {
			t.Fatalf("CreateNode(%s): %v", n.Name, err)
		}
	}

	// m1: a -> b (b is target). m2: b -> c (b is source).
	m1 := migrationParams(uuid.New(), a.ID, b.ID)
	m2 := migrationParams(uuid.New(), b.ID, c.ID)
	if _, err := s.CreateMigration(ctx, m1, migrationJobArgsStub{TaskID: m1.Task.ID, MigrationID: m1.ID}); err != nil {
		t.Fatalf("CreateMigration(m1): %v", err)
	}
	if _, err := s.CreateMigration(ctx, m2, migrationJobArgsStub{TaskID: m2.Task.ID, MigrationID: m2.ID}); err != nil {
		t.Fatalf("CreateMigration(m2): %v", err)
	}

	got, err := s.ListMigrations(ctx, store.ListMigrationsParams{LimitCount: 50, NodeID: &b.ID})
	if err != nil {
		t.Fatalf("ListMigrations(NodeID=b): %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListMigrations(NodeID=b) returned %d, want 2 (source-or-target)", len(got))
	}
	seen := map[uuid.UUID]bool{}
	for _, m := range got {
		seen[m.ID] = true
	}
	if !seen[m1.ID] || !seen[m2.ID] {
		t.Errorf("ListMigrations(NodeID=b) = %+v, want both m1=%v and m2=%v", got, m1.ID, m2.ID)
	}
}

func TestListMigrations_OrderingAndLimit(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	src := nodeParams(uniqueNodeName("src"))
	tgt := nodeParams(uniqueNodeName("tgt"))
	if _, err := s.CreateNode(ctx, src); err != nil {
		t.Fatalf("CreateNode(source): %v", err)
	}
	if _, err := s.CreateNode(ctx, tgt); err != nil {
		t.Fatalf("CreateNode(target): %v", err)
	}

	// Seed three migrations across distinct VMs so the per-VM active guard does
	// not reject them.
	var created []store.Migration
	for i := 0; i < 3; i++ {
		p := migrationParams(uuid.New(), src.ID, tgt.ID)
		m, err := s.CreateMigration(ctx, p, migrationJobArgsStub{TaskID: p.Task.ID, MigrationID: p.ID})
		if err != nil {
			t.Fatalf("CreateMigration(%d): %v", i, err)
		}
		created = append(created, m)
		time.Sleep(2 * time.Millisecond)
	}

	all, err := s.ListMigrations(ctx, store.ListMigrationsParams{LimitCount: 50})
	if err != nil {
		t.Fatalf("ListMigrations(all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListMigrations(all) returned %d, want 3", len(all))
	}
	// Newest-first: descending by CreatedAt, tie-break by ID descending.
	for i := 0; i+1 < len(all); i++ {
		if all[i].CreatedAt.Before(all[i+1].CreatedAt) {
			t.Errorf("ListMigrations not newest-first at %d: %v before %v", i, all[i].CreatedAt, all[i+1].CreatedAt)
		}
	}

	limited, err := s.ListMigrations(ctx, store.ListMigrationsParams{LimitCount: 2})
	if err != nil {
		t.Fatalf("ListMigrations(limit=2): %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("ListMigrations(limit=2) returned %d, want 2", len(limited))
	}
	if len(all) >= 2 && (limited[0].ID != all[0].ID || limited[1].ID != all[1].ID) {
		t.Errorf("ListMigrations(limit=2) = %v,%v, want first two of all %v,%v", limited[0].ID, limited[1].ID, all[0].ID, all[1].ID)
	}
}
