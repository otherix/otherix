// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"fmt"
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

// TestDeleteExpiredTasksQuarantinesUndecodableKey pins the L12 quarantine on a
// representative sweep: a malformed task key must not halt DeleteExpiredTasks.
// The valid expired task is still deleted (count 1), no error, and the poison
// key is left in place (audit R2-L12).
func TestDeleteExpiredTasksQuarantinesUndecodableKey(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	// One valid task that is past retention once finalized.
	expired := taskParams(store.TaskStatusPending, nil)
	if _, err := s.EnqueueTask(ctx, expired, testJobArgs{}); err != nil {
		t.Fatalf("EnqueueTask: %v", err)
	}
	if err := s.UpdateTaskFinalized(ctx, store.UpdateTaskFinalizedParams{ID: expired.ID, Status: store.TaskStatusSuccess}); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	// A poison key under the tasks prefix carrying raw non-JSON bytes.
	poisonKey := etcd.Key("tasks", "poison")
	if _, err := cli.Raw().Put(ctx, poisonKey, "not json"); err != nil {
		t.Fatalf("seed poison key: %v", err)
	}

	future := time.Now().UTC().Add(time.Hour)
	deleted, err := s.DeleteExpiredTasks(ctx, store.DeleteExpiredTasksParams{CompletedCutoff: future, FailedCutoff: future})
	if err != nil {
		t.Fatalf("DeleteExpiredTasks error = %v, want nil", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (valid expired task, poison skipped)", deleted)
	}

	// The poison key is quarantined (skipped + logged), never deleted.
	resp, err := cli.Raw().Get(ctx, poisonKey)
	if err != nil {
		t.Fatalf("get poison key: %v", err)
	}
	if len(resp.Kvs) != 1 {
		t.Errorf("poison key count = %d, want 1 (must not be deleted)", len(resp.Kvs))
	}
}

// TestDeleteFailedJobsRespectsStateAndAge seeds four job rows directly and runs
// the sweep with a 7-day cutoff: only the failed job older than the cutoff is
// deleted; recent-failed, running, and pending rows survive.
func TestDeleteFailedJobsRespectsStateAndAge(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	jobKey := func(id int64) string { return etcd.Key("jobs", fmt.Sprintf("%020d", id)) }
	seedJob := func(id int64, state etcdstore.JobState, failedAt *time.Time) {
		j := etcdstore.Job{ID: id, Kind: "test.job", State: state, FailedAt: failedAt}
		if err := cli.PutJSON(ctx, jobKey(id), j); err != nil {
			t.Fatalf("seed job %d: %v", id, err)
		}
	}
	exists := func(id int64) bool {
		var j etcdstore.Job
		found, err := cli.GetJSON(ctx, jobKey(id), &j)
		if err != nil {
			t.Fatalf("get job %d: %v", id, err)
		}
		return found
	}

	now := time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	recent := now.Add(-time.Hour)
	seedJob(1, etcdstore.JobStateFailed, &old)    // deleted
	seedJob(2, etcdstore.JobStateFailed, &recent) // retained (too recent)
	seedJob(3, etcdstore.JobStateRunning, nil)    // retained (never touch running)
	seedJob(4, etcdstore.JobStatePending, nil)    // retained (pending)

	deleted, err := s.DeleteFailedJobs(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("DeleteFailedJobs: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if exists(1) {
		t.Errorf("job 1 (old failed) survived, want swept")
	}
	for _, id := range []int64{2, 3, 4} {
		if !exists(id) {
			t.Errorf("job %d wrongly swept, want retained", id)
		}
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

// TestDeleteOrphanedNetworkNodeStatus seeds a network_node_status row for a live
// network and another for a deleted (non-resolving) network, then runs the
// sweep: the orphan must be deleted and the live-network row must survive.
func TestDeleteOrphanedNetworkNodeStatus(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	liveNet, err := s.CreateNetwork(ctx, netParams(uniqueNetName("nns-live")))
	if err != nil {
		t.Fatalf("CreateNetwork(live): %v", err)
	}
	liveNode := uuid.New()
	if err := s.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
		NetworkID: liveNet.ID, NodeID: liveNode, ReconciliationStatus: "ready",
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus(live): %v", err)
	}

	// Orphan: a status row whose network never existed (no live network resolves).
	orphanNet := uuid.New()
	orphanNode := uuid.New()
	if err := s.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
		NetworkID: orphanNet, NodeID: orphanNode, ReconciliationStatus: "ready",
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus(orphan): %v", err)
	}

	deleted, err := s.DeleteOrphanedNetworkNodeStatus(ctx)
	if err != nil {
		t.Fatalf("DeleteOrphanedNetworkNodeStatus: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (the orphan only)", deleted)
	}

	// The live-network row survives.
	live, err := s.ListNetworkNodeStatusByNetwork(ctx, liveNet.ID)
	if err != nil {
		t.Fatalf("ListNetworkNodeStatusByNetwork(live): %v", err)
	}
	if len(live) != 1 {
		t.Errorf("live network status rows = %d, want 1 (must survive)", len(live))
	}

	// The orphan is gone.
	orphan, err := s.ListNetworkNodeStatusByNetwork(ctx, orphanNet)
	if err != nil {
		t.Fatalf("ListNetworkNodeStatusByNetwork(orphan): %v", err)
	}
	if len(orphan) != 0 {
		t.Errorf("orphan network status rows = %d, want 0 (must be swept)", len(orphan))
	}

	// Idempotent: a second sweep deletes nothing.
	deleted, err = s.DeleteOrphanedNetworkNodeStatus(ctx)
	if err != nil {
		t.Fatalf("DeleteOrphanedNetworkNodeStatus(second): %v", err)
	}
	if deleted != 0 {
		t.Errorf("second sweep deleted = %d, want 0", deleted)
	}
}
