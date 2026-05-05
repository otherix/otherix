// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/otherix/otherix/internal/store"
)

// uniqueNodeName returns a per-call unique name so tests don't collide on
// uq_nodes_name.
func uniqueNodeName() string {
	return "node-" + uuid.NewString()
}

func defaultNodeParams(id uuid.UUID, name string) store.CreateNodeParams {
	return store.CreateNodeParams{
		ID:                      id,
		Name:                    name,
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://" + name + ".otherix.local:9443",
		MigrationHost:           "10.0.0.1",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  store.NodeStatusPending,
	}
}

func TestNodesCreateGetByIDGetByName(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	name := uniqueNodeName()

	created, err := s.Queries().CreateNode(ctx, defaultNodeParams(id, name))
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if created.ID != id {
		t.Errorf("created.ID = %v, want %v", created.ID, id)
	}
	if created.Name != name {
		t.Errorf("created.Name = %q, want %q", created.Name, name)
	}
	if created.Architecture != store.CpuArchAmd64 {
		t.Errorf("created.Architecture = %v, want amd64", created.Architecture)
	}

	if _, err := s.Queries().GetNodeByID(ctx, id); err != nil {
		t.Fatalf("GetNodeByID: %v", err)
	}
	if _, err := s.Queries().GetNodeByName(ctx, name); err != nil {
		t.Fatalf("GetNodeByName: %v", err)
	}
}

func TestNodesDuplicateNameRejected(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	name := uniqueNodeName()
	if _, err := s.Queries().CreateNode(ctx, defaultNodeParams(uuid.New(), name)); err != nil {
		t.Fatalf("first CreateNode: %v", err)
	}
	_, err := s.Queries().CreateNode(ctx, defaultNodeParams(uuid.New(), name))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("duplicate-name err = %v, want pg 23505", err)
	}
}

func TestNodesListWithFilters(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	// Insert 3 amd64 + 2 arm64 nodes, all status=pending.
	for i := 0; i < 3; i++ {
		p := defaultNodeParams(uuid.New(), uniqueNodeName())
		p.Architecture = store.CpuArchAmd64
		if _, err := s.Queries().CreateNode(ctx, p); err != nil {
			t.Fatalf("amd CreateNode %d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		p := defaultNodeParams(uuid.New(), uniqueNodeName())
		p.Architecture = store.CpuArchArm64
		if _, err := s.Queries().CreateNode(ctx, p); err != nil {
			t.Fatalf("arm CreateNode %d: %v", i, err)
		}
	}

	armArch := store.CpuArchArm64
	rows, err := s.Queries().ListNodes(ctx, store.ListNodesParams{
		Architecture: &armArch,
		LimitCount:   100,
	})
	if err != nil {
		t.Fatalf("ListNodes arm64: %v", err)
	}
	gotArm := 0
	for _, r := range rows {
		if r.Architecture == store.CpuArchArm64 {
			gotArm++
		}
		if r.Architecture == store.CpuArchAmd64 {
			t.Errorf("arm64 filter returned amd64 row: %v", r.ID)
		}
	}
	if gotArm < 2 {
		t.Errorf("ListNodes arm64 returned %d arm rows, want >= 2", gotArm)
	}
}

func TestNodesUpdateHeartbeat(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	if _, err := s.Queries().CreateNode(ctx, defaultNodeParams(id, uniqueNodeName())); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	if err := s.Queries().UpdateNodeHeartbeat(ctx, store.UpdateNodeHeartbeatParams{
		ID:                      id,
		AgentVersion:            ptr("v0.1.0"),
		MigrationHost:           "10.0.0.2",
		MigrationPortRangeStart: 50000,
		MigrationPortRangeEnd:   50099,
		CpuCoresTotal:           ptr32(64),
		CpuCoresAvailable:       ptr32(60),
		CpuModel:                ptr("Cool CPU"),
		CpuFlags:                []string{"sse2", "avx"},
		MemoryTotalMib:          ptr64(131072),
		MemoryAvailableMib:      ptr64(120000),
		Hugepages2mibTotal:      ptr32(0),
		Hugepages1gibTotal:      ptr32(0),
		KernelVersion:           ptr("6.6.0"),
		QemuVersion:             ptr("9.0.0"),
		NumaTopology:            []byte(`{"nodes":1}`),
		Capabilities:            []byte(`{"live_migration":true}`),
	}); err != nil {
		t.Fatalf("UpdateNodeHeartbeat: %v", err)
	}

	got, err := s.Queries().GetNodeByID(ctx, id)
	if err != nil {
		t.Fatalf("GetNodeByID: %v", err)
	}
	if got.AgentVersion == nil || *got.AgentVersion != "v0.1.0" {
		t.Errorf("AgentVersion = %v, want v0.1.0", got.AgentVersion)
	}
	if got.LastHeartbeatAt == nil {
		t.Errorf("LastHeartbeatAt = nil after heartbeat, want set")
	}
	if got.MigrationHost != "10.0.0.2" {
		t.Errorf("MigrationHost = %q, want 10.0.0.2", got.MigrationHost)
	}
}

func TestNodesUpdateStatus(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	if _, err := s.Queries().CreateNode(ctx, defaultNodeParams(id, uniqueNodeName())); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	now := time.Now()
	if err := s.Queries().UpdateNodeStatus(ctx, store.UpdateNodeStatusParams{
		ID:         id,
		Status:     store.NodeStatusCordoned,
		CordonedAt: &now,
	}); err != nil {
		t.Fatalf("UpdateNodeStatus to cordoned: %v", err)
	}
	got, err := s.Queries().GetNodeByID(ctx, id)
	if err != nil {
		t.Fatalf("GetNodeByID: %v", err)
	}
	if got.Status != store.NodeStatusCordoned {
		t.Errorf("Status = %v, want cordoned", got.Status)
	}
	if got.CordonedAt == nil {
		t.Errorf("CordonedAt = nil, want set")
	}

	// Clear cordoned_at by setting status back to ready with nil.
	if err := s.Queries().UpdateNodeStatus(ctx, store.UpdateNodeStatusParams{
		ID:         id,
		Status:     store.NodeStatusReady,
		CordonedAt: nil,
	}); err != nil {
		t.Fatalf("UpdateNodeStatus to ready: %v", err)
	}
	got, err = s.Queries().GetNodeByID(ctx, id)
	if err != nil {
		t.Fatalf("GetNodeByID after clear: %v", err)
	}
	if got.Status != store.NodeStatusReady {
		t.Errorf("Status = %v, want ready", got.Status)
	}
	if got.CordonedAt != nil {
		t.Errorf("CordonedAt = %v, want nil", got.CordonedAt)
	}
}

func TestNodesSoftDeleteHidesFromGet(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	id := uuid.New()
	if _, err := s.Queries().CreateNode(ctx, defaultNodeParams(id, uniqueNodeName())); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if err := s.Queries().SoftDeleteNode(ctx, id); err != nil {
		t.Fatalf("SoftDeleteNode: %v", err)
	}
	if _, err := s.Queries().GetNodeByID(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetNodeByID after soft delete err = %v, want pgx.ErrNoRows", err)
	}
}

func TestNodesListStaleNodes(t *testing.T) {
	requireSharedHarness(t)
	ctx := context.Background()
	s := newStore(t, sharedHarness)

	// Stale: never heartbeated.
	staleID := uuid.New()
	if _, err := s.Queries().CreateNode(ctx, defaultNodeParams(staleID, uniqueNodeName())); err != nil {
		t.Fatalf("CreateNode stale: %v", err)
	}

	// Fresh: heartbeat now.
	freshID := uuid.New()
	if _, err := s.Queries().CreateNode(ctx, defaultNodeParams(freshID, uniqueNodeName())); err != nil {
		t.Fatalf("CreateNode fresh: %v", err)
	}
	if err := s.Queries().UpdateNodeHeartbeat(ctx, store.UpdateNodeHeartbeatParams{
		ID:                      freshID,
		MigrationHost:           "10.0.0.3",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		CpuFlags:                []string{},
		Capabilities:            []byte(`{}`),
	}); err != nil {
		t.Fatalf("UpdateNodeHeartbeat fresh: %v", err)
	}

	cutoff := time.Now().Add(-time.Minute)
	stale, err := s.Queries().ListStaleNodes(ctx, cutoff)
	if err != nil {
		t.Fatalf("ListStaleNodes: %v", err)
	}

	sawStale, sawFresh := false, false
	for _, n := range stale {
		if n.ID == staleID {
			sawStale = true
		}
		if n.ID == freshID {
			sawFresh = true
		}
	}
	if !sawStale {
		t.Errorf("stale node not in ListStaleNodes")
	}
	if sawFresh {
		t.Errorf("fresh node returned by ListStaleNodes")
	}
}

func ptr[T any](v T) *T    { return &v }
func ptr32(v int32) *int32 { return &v }
func ptr64(v int64) *int64 { return &v }
