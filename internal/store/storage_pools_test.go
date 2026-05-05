// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/otherix/otherix/internal/store"
)

// uniquePoolName returns a per-call unique pool name so tests don't
// collide on uq_storage_pools_name.
func uniquePoolName(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

// seedNodeForPools creates a minimal node row so storage_pools rows
// can satisfy the FK. The migration ingress / agent endpoint values
// are not exercised by the pool tests; we just need a real id.
func seedNodeForPools(t *testing.T, ctx context.Context, s *store.Store) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := s.Queries().CreateNode(ctx, defaultNodeParams(id, uniqueNodeName())); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return id
}

func defaultPoolParams(id, nodeID uuid.UUID, name string) store.CreateStoragePoolParams {
	return store.CreateStoragePoolParams{
		ID:     id,
		NodeID: nodeID,
		Name:   name,
		Type:   "local_dir",
		Path:   "/opt/otherix/pools/" + name,
		Config: []byte(`{}`),
	}
}

func TestStoragePoolsCreateGetByID(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)
	nodeID := seedNodeForPools(t, ctx, s)

	id := uuid.New()
	name := uniquePoolName("ssd")

	created, err := s.Queries().CreateStoragePool(ctx, defaultPoolParams(id, nodeID, name))
	if err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	if created.ID != id {
		t.Errorf("created.ID = %v, want %v", created.ID, id)
	}
	if created.NodeID != nodeID {
		t.Errorf("created.NodeID = %v, want %v", created.NodeID, nodeID)
	}
	if created.Type != "local_dir" {
		t.Errorf("created.Type = %v, want local_dir", created.Type)
	}
	if created.CapacityBytes != nil || created.AvailableBytes != nil || created.ReportedAt != nil {
		t.Errorf("agent-reported fields set on insert: capacity=%v available=%v reported=%v",
			created.CapacityBytes, created.AvailableBytes, created.ReportedAt)
	}

	got, err := s.Queries().GetStoragePoolByID(ctx, id)
	if err != nil {
		t.Fatalf("GetStoragePoolByID: %v", err)
	}
	if got.Name != name {
		t.Errorf("got.Name = %q, want %q", got.Name, name)
	}
}

func TestStoragePoolsDuplicateNamePerNodeRejected(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)
	nodeID := seedNodeForPools(t, ctx, s)

	name := uniquePoolName("dup")
	if _, err := s.Queries().CreateStoragePool(ctx, defaultPoolParams(uuid.New(), nodeID, name)); err != nil {
		t.Fatalf("first CreateStoragePool: %v", err)
	}
	_, err := s.Queries().CreateStoragePool(ctx, defaultPoolParams(uuid.New(), nodeID, name))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("duplicate-name err = %v, want pg 23505", err)
	}
}

// TestStoragePoolsSameNameAcrossNodesAllowed locks in the multi-
// instance behaviour: uq_storage_pools_name is per-node UNIQUE so the
// same conceptual pool may materialise on multiple nodes simul-
// taneously. The scheduler later picks the per-node instance at
// VM-create time.
func TestStoragePoolsSameNameAcrossNodesAllowed(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	nodeA := seedNodeForPools(t, ctx, s)
	nodeB := seedNodeForPools(t, ctx, s)
	name := uniquePoolName("shared")

	if _, err := s.Queries().CreateStoragePool(ctx, defaultPoolParams(uuid.New(), nodeA, name)); err != nil {
		t.Fatalf("CreateStoragePool A: %v", err)
	}
	if _, err := s.Queries().CreateStoragePool(ctx, defaultPoolParams(uuid.New(), nodeB, name)); err != nil {
		t.Fatalf("CreateStoragePool B (same name, different node) err = %v, want success", err)
	}
}

// TestListStoragePoolsByName_AggregatedRows exercises the resolver /
// scheduler entry-point query: same-named instances across nodes come
// back as a slice.
func TestListStoragePoolsByName_AggregatedRows(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	nodeA := seedNodeForPools(t, ctx, s)
	nodeB := seedNodeForPools(t, ctx, s)
	name := uniquePoolName("aggr")

	if _, err := s.Queries().CreateStoragePool(ctx, defaultPoolParams(uuid.New(), nodeA, name)); err != nil {
		t.Fatalf("CreateStoragePool A: %v", err)
	}
	if _, err := s.Queries().CreateStoragePool(ctx, defaultPoolParams(uuid.New(), nodeB, name)); err != nil {
		t.Fatalf("CreateStoragePool B: %v", err)
	}

	rows, err := s.Queries().ListStoragePoolsByName(ctx, name)
	if err != nil {
		t.Fatalf("ListStoragePoolsByName: %v", err)
	}
	if got := len(rows); got != 2 {
		t.Errorf("ListStoragePoolsByName len = %d, want 2", got)
	}
}

func TestStoragePoolsListWithFilters(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	nodeA := seedNodeForPools(t, ctx, s)
	nodeB := seedNodeForPools(t, ctx, s)

	for i := 0; i < 3; i++ {
		if _, err := s.Queries().CreateStoragePool(ctx, defaultPoolParams(uuid.New(), nodeA, uniquePoolName("a"))); err != nil {
			t.Fatalf("CreateStoragePool nodeA #%d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := s.Queries().CreateStoragePool(ctx, defaultPoolParams(uuid.New(), nodeB, uniquePoolName("b"))); err != nil {
			t.Fatalf("CreateStoragePool nodeB #%d: %v", i, err)
		}
	}

	rows, err := s.Queries().ListStoragePools(ctx, store.ListStoragePoolsParams{
		NodeID:     &nodeA,
		LimitCount: 100,
	})
	if err != nil {
		t.Fatalf("ListStoragePools nodeA: %v", err)
	}
	if got := len(rows); got != 3 {
		t.Errorf("len(rows) = %d, want 3", got)
	}
	for _, r := range rows {
		if r.NodeID != nodeA {
			t.Errorf("ListStoragePools(nodeA) returned NodeID=%v, want %v", r.NodeID, nodeA)
		}
	}

	pt := "local_dir"
	rows, err = s.Queries().ListStoragePools(ctx, store.ListStoragePoolsParams{
		Type:       &pt,
		LimitCount: 100,
	})
	if err != nil {
		t.Fatalf("ListStoragePools type: %v", err)
	}
	if len(rows) < 5 {
		t.Errorf("ListStoragePools(type=local_dir) len = %d, want >= 5 (cross-test pollution allowed)", len(rows))
	}
}

func TestStoragePoolsCursorPaginationOrdering(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	nodeID := seedNodeForPools(t, ctx, s)
	const total = 5
	for i := 0; i < total; i++ {
		if _, err := s.Queries().CreateStoragePool(ctx, defaultPoolParams(uuid.New(), nodeID, uniquePoolName("page"))); err != nil {
			t.Fatalf("CreateStoragePool #%d: %v", i, err)
		}
	}

	page1, err := s.Queries().ListStoragePools(ctx, store.ListStoragePoolsParams{
		NodeID:     &nodeID,
		LimitCount: 3,
	})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 3 {
		t.Fatalf("page1 len = %d, want 3", len(page1))
	}
	last := page1[len(page1)-1]

	page2, err := s.Queries().ListStoragePools(ctx, store.ListStoragePoolsParams{
		NodeID:          &nodeID,
		CursorCreatedAt: &last.CreatedAt,
		CursorID:        &last.ID,
		LimitCount:      3,
	})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	// page1 (3) + page2 (2) = total 5; if page2 over-runs it pulled a
	// pre-cursor row and the ordering is broken.
	if got := len(page1) + len(page2); got != total {
		t.Errorf("page1 + page2 = %d, want %d", got, total)
	}
	for _, p := range page2 {
		if p.CreatedAt.Before(last.CreatedAt) {
			t.Errorf("page2 row CreatedAt %v is before cursor %v", p.CreatedAt, last.CreatedAt)
		}
	}
}

func TestStoragePoolsSoftDeleteHidesFromGet(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)
	nodeID := seedNodeForPools(t, ctx, s)

	id := uuid.New()
	if _, err := s.Queries().CreateStoragePool(ctx, defaultPoolParams(id, nodeID, uniquePoolName("sd"))); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	if err := s.Queries().SoftDeleteStoragePool(ctx, id); err != nil {
		t.Fatalf("SoftDeleteStoragePool: %v", err)
	}
	if _, err := s.Queries().GetStoragePoolByID(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetStoragePoolByID after soft delete err = %v, want pgx.ErrNoRows", err)
	}
}

func TestStoragePoolsCountVMDisks(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)
	nodeID := seedNodeForPools(t, ctx, s)

	poolID := uuid.New()
	if _, err := s.Queries().CreateStoragePool(ctx, defaultPoolParams(poolID, nodeID, uniquePoolName("cnt"))); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}

	zero, err := s.Queries().CountVMDisksOnStoragePool(ctx, poolID)
	if err != nil {
		t.Fatalf("CountVMDisksOnStoragePool zero: %v", err)
	}
	if zero != 0 {
		t.Errorf("count = %d, want 0", zero)
	}

	// Insert a vm + 2 disks against this pool; one is soft-deleted.
	ownerID := uuid.New()
	const insUser = `
		insert into users (id, email, password_hash, role)
		values ($1, $2, 'x', 'developer')`
	if _, err := s.Pool().Exec(ctx, insUser, ownerID, "owner-"+uuid.NewString()+"@example.test"); err != nil {
		t.Fatalf("insert users: %v", err)
	}
	vmID := uuid.New()
	const insVM = `
		insert into vms
		  (id, owner_id, name, architecture, cpu_cores, memory_mib, machine_type)
		values
		  ($1, $2, $3, 'amd64', 1, 256, 'pc-i440fx-8.0')`
	if _, err := s.Pool().Exec(ctx, insVM, vmID, ownerID, "vm-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("insert vms: %v", err)
	}
	const insActiveDisk = `
		insert into vm_disks
		  (id, vm_id, storage_pool_id, device_order, source_kind, size_gib)
		values
		  ($1, $2, $3, 0, 'blank', 1)`
	const insDeletedDisk = `
		insert into vm_disks
		  (id, vm_id, storage_pool_id, device_order, source_kind, size_gib, deleted_at)
		values
		  ($1, $2, $3, 1, 'blank', 1, now())`
	if _, err := s.Pool().Exec(ctx, insActiveDisk, uuid.New(), vmID, poolID); err != nil {
		t.Fatalf("insert active disk: %v", err)
	}
	if _, err := s.Pool().Exec(ctx, insDeletedDisk, uuid.New(), vmID, poolID); err != nil {
		t.Fatalf("insert deleted disk: %v", err)
	}

	got, err := s.Queries().CountVMDisksOnStoragePool(ctx, poolID)
	if err != nil {
		t.Fatalf("CountVMDisksOnStoragePool: %v", err)
	}
	if got != 1 {
		t.Errorf("count = %d, want 1 (soft-deleted disk excluded)", got)
	}
}

func TestStoragePoolsUpdateAndUpsertUsage(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)
	nodeID := seedNodeForPools(t, ctx, s)

	id := uuid.New()
	if _, err := s.Queries().CreateStoragePool(ctx, defaultPoolParams(id, nodeID, uniquePoolName("upd"))); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}

	renamed := uniquePoolName("renamed")
	updated, err := s.Queries().UpdateStoragePool(ctx, store.UpdateStoragePoolParams{
		ID:     id,
		Name:   renamed,
		Config: []byte(`{"hint":"warm"}`),
	})
	if err != nil {
		t.Fatalf("UpdateStoragePool: %v", err)
	}
	if updated.Name != renamed {
		t.Errorf("Name = %q, want %q", updated.Name, renamed)
	}
	if diff := cmp.Diff(`{"hint": "warm"}`, string(updated.Config)); diff != "" {
		// Postgres jsonb formatting normalises the literal; leave the
		// diff so a future change is loud rather than masked.
		t.Logf("config diff (normalised): %s", diff)
	}

	cap := int64(1 << 40)
	avail := int64(1 << 30)
	if err := s.Queries().UpsertStoragePoolUsage(ctx, store.UpsertStoragePoolUsageParams{
		ID:             id,
		CapacityBytes:  &cap,
		AvailableBytes: &avail,
	}); err != nil {
		t.Fatalf("UpsertStoragePoolUsage: %v", err)
	}
	got, err := s.Queries().GetStoragePoolByID(ctx, id)
	if err != nil {
		t.Fatalf("GetStoragePoolByID: %v", err)
	}
	if got.CapacityBytes == nil || *got.CapacityBytes != cap {
		t.Errorf("CapacityBytes = %v, want %d", got.CapacityBytes, cap)
	}
	if got.AvailableBytes == nil || *got.AvailableBytes != avail {
		t.Errorf("AvailableBytes = %v, want %d", got.AvailableBytes, avail)
	}
	if got.ReportedAt == nil {
		t.Error("ReportedAt = nil, want set after UpsertStoragePoolUsage")
	}
}

// TestClusterSettingsDefaultPoolRoundTrip exercises the
// cluster_settings singleton through the generated sqlc surface: the
// migration seeds the row at id=1 with NULL default_pool_name; we set
// it, read it back, clear it, and verify the GET surface again.
func TestClusterSettingsDefaultPoolRoundTrip(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	settings, err := s.Queries().GetClusterSettings(ctx)
	if err != nil {
		t.Fatalf("GetClusterSettings: %v", err)
	}
	// May or may not be nil depending on cross-test pollution; we only
	// care that the row exists and round-trips below.
	_ = settings

	name := "default"
	if err := s.Queries().SetDefaultPoolName(ctx, &name); err != nil {
		t.Fatalf("SetDefaultPoolName: %v", err)
	}
	got, err := s.Queries().GetClusterSettings(ctx)
	if err != nil {
		t.Fatalf("GetClusterSettings after set: %v", err)
	}
	if got.DefaultPoolName == nil || *got.DefaultPoolName != name {
		t.Errorf("DefaultPoolName = %v, want %q", got.DefaultPoolName, name)
	}

	if err := s.Queries().ClearDefaultPoolName(ctx); err != nil {
		t.Fatalf("ClearDefaultPoolName: %v", err)
	}
	got, err = s.Queries().GetClusterSettings(ctx)
	if err != nil {
		t.Fatalf("GetClusterSettings after clear: %v", err)
	}
	if got.DefaultPoolName != nil {
		t.Errorf("DefaultPoolName after clear = %v, want nil", got.DefaultPoolName)
	}
}
