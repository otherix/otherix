// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// bumpHeartbeat marks a node's last_heartbeat_at fresh via the heartbeat hp.
func bumpHeartbeat(t *testing.T, s *etcdstore.Store, nodeID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	cores := int32(8)
	mem := int64(16384)
	if err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		return hp.UpdateNodeHeartbeat(ctx, store.UpdateNodeHeartbeatParams{
			ID: nodeID, MigrationHost: "10.0.0.1", MigrationPortRangeStart: 49152, MigrationPortRangeEnd: 49251,
			CPUCoresTotal: &cores, CPUCoresAvailable: &cores, MemoryTotalMib: &mem, MemoryAvailableMib: &mem,
		})
	}); err != nil {
		t.Fatalf("bumpHeartbeat: %v", err)
	}
}

func TestDeleteExpiredTasks(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	creator := uuid.New()

	success := taskParams(store.TaskStatusPending, &creator)
	failed := taskParams(store.TaskStatusPending, nil)
	pending := taskParams(store.TaskStatusPending, nil)
	for _, p := range []store.CreateTaskParams{success, failed, pending} {
		if _, err := s.EnqueueTask(ctx, p, testJobArgs{}); err != nil {
			t.Fatalf("EnqueueTask: %v", err)
		}
	}
	if err := s.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: success.ID, Status: store.TaskStatusSuccess}); err != nil {
		t.Fatalf("finalize success: %v", err)
	}
	if err := s.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: failed.ID, Status: store.TaskStatusFailed}); err != nil {
		t.Fatalf("finalize failed: %v", err)
	}

	// Cutoffs in the near future: both finalized tasks are past retention; the
	// pending task (no finished_at) survives.
	future := time.Now().UTC().Add(time.Hour)
	deleted, err := s.DeleteExpiredTasks(ctx, store.DeleteExpiredTasksParams{CompletedCutoff: future, FailedCutoff: future})
	if err != nil {
		t.Fatalf("DeleteExpiredTasks: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
	if _, err := s.TaskByID(ctx, success.ID); err == nil {
		t.Errorf("success task survived retention")
	}
	if _, err := s.TaskByID(ctx, pending.ID); err != nil {
		t.Errorf("pending task wrongly deleted: %v", err)
	}
	// created-by index dropped: own-scope listing no longer sees the success task.
	own, _ := s.ListTasksOwn(ctx, store.ListTasksOwnParams{CreatedBy: &creator, LimitCount: 50})
	if len(own) != 0 {
		t.Errorf("own tasks after retention = %d, want 0 (index dropped)", len(own))
	}
}

func TestPromoteHealthyNodes(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	np := nodeParams(uniqueNodeName("promote"))
	if _, err := s.CreateNode(ctx, np); err != nil { // lands pending
		t.Fatalf("CreateNode: %v", err)
	}
	bumpHeartbeat(t, s, np.ID)

	freshAfter := time.Now().UTC().Add(-time.Minute)
	rows, err := s.PromoteHealthyNodes(ctx, freshAfter)
	if err != nil {
		t.Fatalf("PromoteHealthyNodes: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != np.ID || rows[0].Status != store.NodeStatusReady {
		t.Fatalf("promoted = %+v, want one ready row for %v", rows, np.ID)
	}
	n, _ := s.NodeByID(ctx, np.ID)
	if n.Status != store.NodeStatusReady {
		t.Errorf("node status = %v, want ready", n.Status)
	}

	// A second pass is a no-op (already ready).
	rows, _ = s.PromoteHealthyNodes(ctx, freshAfter)
	if len(rows) != 0 {
		t.Errorf("second promote = %d, want 0", len(rows))
	}
}

func TestMarkNodesUnreachable(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	np := nodeParams(uniqueNodeName("stale"))
	if _, err := s.CreateNode(ctx, np); err != nil { // pending, no heartbeat
		t.Fatalf("CreateNode: %v", err)
	}

	rows, err := s.MarkNodesUnreachable(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("MarkNodesUnreachable: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != np.ID {
		t.Fatalf("marked = %+v, want one row for %v", rows, np.ID)
	}
	n, _ := s.NodeByID(ctx, np.ID)
	if n.Status != store.NodeStatusUnreachable {
		t.Errorf("node status = %v, want unreachable", n.Status)
	}
}

func TestMarkNodesGone(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	// seedNode writes a node row directly so the test controls status and
	// last_heartbeat_at precisely.
	seedNode := func(name string, status store.NodeStatus, hb *time.Time) uuid.UUID {
		id := uuid.New()
		n := store.Node{
			ID:                 id,
			Name:               uniqueNodeName(name),
			Architecture:       store.CpuArchAmd64,
			AdvertisedEndpoint: "https://node.example:9443",
			MigrationHost:      "10.0.0.1",
			Status:             status,
			LastHeartbeatAt:    hb,
			CreatedAt:          time.Now().UTC(),
			UpdatedAt:          time.Now().UTC(),
		}
		if err := cli.PutJSON(ctx, etcd.Key("nodes", id.String()), n); err != nil {
			t.Fatalf("seed node %q: %v", name, err)
		}
		return id
	}

	old := time.Now().Add(-10 * time.Minute)
	recent := time.Now().Add(-30 * time.Second)
	// unreachable + stale-past-grace -> gone
	gone := seedNode("n-gone", store.NodeStatusUnreachable, &old)
	// unreachable but heartbeat newer than the grace cutoff -> stays
	staysUnreachable := seedNode("n-stay", store.NodeStatusUnreachable, &recent)
	// ready -> never touched by MarkNodesGone
	staysReady := seedNode("n-ready", store.NodeStatusReady, &old)

	goneBefore := time.Now().Add(-5 * time.Minute)
	rows, err := s.MarkNodesGone(ctx, goneBefore)
	if err != nil {
		t.Fatalf("MarkNodesGone: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != gone {
		t.Fatalf("rows = %+v, want exactly the gone node %s", rows, gone)
	}
	if n, _ := s.NodeByID(ctx, gone); n.Status != store.NodeStatusGone {
		t.Errorf("gone node status = %v, want gone", n.Status)
	}
	if n, _ := s.NodeByID(ctx, staysUnreachable); n.Status != store.NodeStatusUnreachable {
		t.Errorf("stay node status = %v, want unreachable", n.Status)
	}
	if n, _ := s.NodeByID(ctx, staysReady); n.Status != store.NodeStatusReady {
		t.Errorf("ready node status = %v, want ready", n.Status)
	}
}

func TestListPoolsNeedingScan(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	np := nodeParams(uniqueNodeName("needscan"))
	if _, err := s.CreateNode(ctx, np); err != nil { // pending = scannable
		t.Fatalf("CreateNode: %v", err)
	}
	pp := poolParams(np.ID, uniquePoolName("needscan"))
	if _, err := s.CreateStoragePool(ctx, pp); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}

	rows, err := s.ListPoolsNeedingScan(ctx)
	if err != nil {
		t.Fatalf("ListPoolsNeedingScan: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != pp.ID || rows[0].NodeID != np.ID {
		t.Fatalf("rows = %+v, want one for pool %v", rows, pp.ID)
	}

	// An in-flight scan task for the pool removes it from the list.
	scanTask := store.CreateTaskParams{
		ID: uuid.New(), Type: "storage_pool.scan", Status: store.TaskStatusRunning,
		ResourceType: "storage_pool", ResourceID: &pp.ID, Args: []byte(`{}`), MaxAttempts: 25,
	}
	if _, err := s.EnqueueTask(ctx, scanTask, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask(scan): %v", err)
	}
	rows, _ = s.ListPoolsNeedingScan(ctx)
	if len(rows) != 0 {
		t.Errorf("with in-flight scan, rows = %d, want 0", len(rows))
	}
}
