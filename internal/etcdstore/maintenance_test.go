// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

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

// TestDeleteExpiredTasksQuarantinesUndecodableKey pins the quarantine behavior on a
// representative sweep: a malformed task key must not halt DeleteExpiredTasks.
// The valid expired task is still deleted (count 1), no error, and the poison
// key is left in place.
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

// TestReclaimStaleRunningJobs seeds five job rows directly and runs the reaper
// with an olderThan cutoff: a running job with an old ClaimedAt and a running job
// with a nil ClaimedAt (legacy/pre-upgrade) are both reclaimed to pending with
// Attempts UNCHANGED and ClaimedAt cleared; a running job with a recent
// ClaimedAt, a pending job, and a failed job are all left untouched.
func TestReclaimStaleRunningJobs(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	jobKey := func(id int64) string { return etcd.Key("jobs", fmt.Sprintf("%020d", id)) }
	seedJob := func(id int64, state etcdstore.JobState, claimedAt *time.Time, attempts int32) {
		j := etcdstore.Job{ID: id, Kind: "test.job", State: state, Attempts: attempts, ClaimedAt: claimedAt}
		if err := cli.PutJSON(ctx, jobKey(id), j); err != nil {
			t.Fatalf("seed job %d: %v", id, err)
		}
	}
	readJob := func(id int64) etcdstore.Job {
		var j etcdstore.Job
		found, err := cli.GetJSON(ctx, jobKey(id), &j)
		if err != nil {
			t.Fatalf("get job %d: %v", id, err)
		}
		if !found {
			t.Fatalf("job %d missing", id)
		}
		return j
	}

	now := time.Now().UTC()
	old := now.Add(-5 * time.Minute)
	recent := now.Add(-10 * time.Second)
	cutoff := now.Add(-time.Minute)

	seedJob(1, etcdstore.JobStateRunning, &old, 2)    // reclaim (stale)
	seedJob(2, etcdstore.JobStateRunning, &recent, 0) // keep (fresh lease)
	seedJob(3, etcdstore.JobStateRunning, nil, 1)     // reclaim (legacy nil)
	seedJob(4, etcdstore.JobStatePending, nil, 0)     // keep (pending)
	seedJob(5, etcdstore.JobStateFailed, nil, 3)      // keep (failed)

	n, err := s.ReclaimStaleRunningJobs(ctx, cutoff)
	if err != nil {
		t.Fatalf("ReclaimStaleRunningJobs: %v", err)
	}
	if n != 2 {
		t.Errorf("reclaimed = %d, want 2", n)
	}

	// Reclaimed: pending, ClaimedAt cleared, Attempts UNCHANGED.
	if j := readJob(1); j.State != etcdstore.JobStatePending || j.ClaimedAt != nil || j.Attempts != 2 {
		t.Errorf("job 1 = %+v, want pending/nil-ClaimedAt/attempts=2", j)
	}
	if j := readJob(3); j.State != etcdstore.JobStatePending || j.ClaimedAt != nil || j.Attempts != 1 {
		t.Errorf("job 3 = %+v, want pending/nil-ClaimedAt/attempts=1", j)
	}

	// Untouched.
	if j := readJob(2); j.State != etcdstore.JobStateRunning || j.ClaimedAt == nil {
		t.Errorf("job 2 = %+v, want running with fresh ClaimedAt", j)
	}
	if j := readJob(4); j.State != etcdstore.JobStatePending {
		t.Errorf("job 4 = %+v, want pending", j)
	}
	if j := readJob(5); j.State != etcdstore.JobStateFailed {
		t.Errorf("job 5 = %+v, want failed", j)
	}

	// The reclaimed jobs are visible via PendingJobs.
	pending, _ := s.PendingJobs(ctx)
	seen := make(map[int64]bool)
	for _, j := range pending {
		seen[j.ID] = true
	}
	if !seen[1] || !seen[3] || !seen[4] {
		t.Errorf("PendingJobs = %+v, want ids 1,3,4 present", pending)
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

// TestPromoteHealthyNodes_CarriesGatewayRole is the seam guard for the default-
// pool ready-hook: the promotion row must carry the node's committed gateway
// role bit so EnsureDefaultPoolsFunc can skip a gateway-only node (owning a pool
// is exactly what derives the hypervisor role, so a gateway must own none).
func TestPromoteHealthyNodes_CarriesGatewayRole(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	gw := nodeParams(uniqueNodeName("promote-gw"))
	gw.Gateway = true
	if _, err := s.CreateNode(ctx, gw); err != nil {
		t.Fatalf("CreateNode gateway: %v", err)
	}
	hyper := nodeParams(uniqueNodeName("promote-hyper"))
	if _, err := s.CreateNode(ctx, hyper); err != nil {
		t.Fatalf("CreateNode hypervisor: %v", err)
	}
	bumpHeartbeat(t, s, gw.ID)
	bumpHeartbeat(t, s, hyper.ID)

	rows, err := s.PromoteHealthyNodes(ctx, time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatalf("PromoteHealthyNodes: %v", err)
	}

	got := map[uuid.UUID]bool{}
	for _, r := range rows {
		got[r.ID] = r.GatewayRole
	}
	if role, ok := got[gw.ID]; !ok || !role {
		t.Errorf("gateway node row GatewayRole = %v (present=%v), want true", role, ok)
	}
	if role, ok := got[hyper.ID]; !ok || role {
		t.Errorf("hypervisor node row GatewayRole = %v (present=%v), want false", role, ok)
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

// TestMarkNodesGoneNeverHeartbeated pins the grace window for a node that has
// never sent a heartbeat: its gone-eligibility is measured from created_at, not
// treated as infinitely stale. A freshly-joined node that is slow to send its
// first heartbeat must stay in the reversible 'unreachable' state until the full
// grace window elapses from creation - never reaped straight to the terminal
// 'gone' before it has had a chance to report in.
func TestMarkNodesGoneNeverHeartbeated(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()

	seedAt := func(name string, status store.NodeStatus, createdAt time.Time) uuid.UUID {
		id := uuid.New()
		n := store.Node{
			ID:                 id,
			Name:               uniqueNodeName(name),
			Architecture:       store.CpuArchAmd64,
			AdvertisedEndpoint: "https://node.example:9443",
			MigrationHost:      "10.0.0.1",
			Status:             status,
			LastHeartbeatAt:    nil, // never heartbeated
			CreatedAt:          createdAt,
			UpdatedAt:          createdAt,
		}
		if err := cli.PutJSON(ctx, etcd.Key("nodes", id.String()), n); err != nil {
			t.Fatalf("seed node %q: %v", name, err)
		}
		return id
	}

	// unreachable, never heartbeated, created inside the grace window -> stays.
	fresh := seedAt("n-fresh", store.NodeStatusUnreachable, time.Now().Add(-30*time.Second))
	// unreachable, never heartbeated, created before the grace window -> gone.
	old := seedAt("n-old", store.NodeStatusUnreachable, time.Now().Add(-10*time.Minute))

	goneBefore := time.Now().Add(-5 * time.Minute)
	rows, err := s.MarkNodesGone(ctx, goneBefore)
	if err != nil {
		t.Fatalf("MarkNodesGone: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != old {
		t.Fatalf("rows = %+v, want exactly the old node %s", rows, old)
	}
	if n, _ := s.NodeByID(ctx, fresh); n.Status != store.NodeStatusUnreachable {
		t.Errorf("fresh never-heartbeated node status = %v, want unreachable (within grace)", n.Status)
	}
	if n, _ := s.NodeByID(ctx, old); n.Status != store.NodeStatusGone {
		t.Errorf("old never-heartbeated node status = %v, want gone (past grace)", n.Status)
	}
}

// TestMarkNodesGoneSkipsHeartbeatInWindow proves MarkNodesGone re-validates
// staleness on the FRESH read: a heartbeat that lands between the liveNodes
// snapshot and the per-node gone write refreshes last_heartbeat_at, and the node
// must NOT be reaped to the terminal 'gone'. The racing writer is a
// status-guarded promote (unreachable -> ready + fresh heartbeat, a no-op if the
// node is no longer unreachable), so a final "gone with a fresh heartbeat" state
// can only arise from MarkNodesGone clobbering a promote that had already
// committed - the pre-fix bug (casNodeStatus re-read fresh but never re-checked
// staleness).
func TestMarkNodesGoneSkipsHeartbeatInWindow(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	goneBefore := time.Now().Add(-5 * time.Minute)
	old := time.Now().Add(-10 * time.Minute)

	seed := func(name string, hb time.Time) uuid.UUID {
		id := uuid.New()
		n := store.Node{
			ID: id, Name: uniqueNodeName(name), Architecture: store.CpuArchAmd64,
			AdvertisedEndpoint: "https://node.example:9443", MigrationHost: "10.0.0.1",
			Status: store.NodeStatusUnreachable, LastHeartbeatAt: &hb,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := cli.PutJSON(ctx, etcd.Key("nodes", id.String()), n); err != nil {
			t.Fatalf("seed node %q: %v", name, err)
		}
		return id
	}

	// promote refreshes the heartbeat and flips unreachable -> ready under a
	// ModRevision CAS, refusing if the node is no longer unreachable.
	promote := func(id uuid.UUID) {
		key := etcd.Key("nodes", id.String())
		resp, err := cli.Raw().Get(ctx, key)
		if err != nil || len(resp.Kvs) == 0 {
			return
		}
		var n store.Node
		if err := json.Unmarshal(resp.Kvs[0].Value, &n); err != nil {
			return
		}
		if n.Status != store.NodeStatusUnreachable {
			return
		}
		now := time.Now().UTC()
		n.Status = store.NodeStatusReady
		n.LastHeartbeatAt = &now
		n.UpdatedAt = now
		val, err := etcd.Marshal(n)
		if err != nil {
			return
		}
		_, _ = cli.Raw().Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(key), "=", resp.Kvs[0].ModRevision)).
			Then(clientv3.OpPut(key, string(val))).
			Commit()
	}

	// Decoy stale-unreachable nodes lengthen MarkNodesGone's per-node loop after
	// the liveNodes snapshot, widening the window in which the racing promote can
	// land before the target's own gone write.
	for d := 0; d < 8; d++ {
		seed(fmt.Sprintf("decoy%d", d), old)
	}

	for i := 0; i < 60; i++ {
		id := seed(fmt.Sprintf("race%d", i), old)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = s.MarkNodesGone(ctx, goneBefore) }()
		go func() { defer wg.Done(); promote(id) }()
		wg.Wait()

		n, err := s.NodeByID(ctx, id)
		if err != nil {
			t.Fatalf("iter %d NodeByID: %v", i, err)
		}
		if n.Status == store.NodeStatusGone && n.LastHeartbeatAt != nil && !n.LastHeartbeatAt.Before(goneBefore) {
			t.Fatalf("iter %d: node reaped 'gone' despite a fresh heartbeat (hb=%v, goneBefore=%v)",
				i, n.LastHeartbeatAt, goneBefore)
		}
	}
}

// seedNodeRow writes a node row directly, so a test controls status,
// last_heartbeat_at, and drain_task_id precisely.
func seedNodeRow(t *testing.T, cli etcdPutter, name string, status store.NodeStatus, hb *time.Time, drainTaskID *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	n := store.Node{
		ID:                 id,
		Name:               uniqueNodeName(name),
		Architecture:       store.CpuArchAmd64,
		AdvertisedEndpoint: "https://node.example:9443",
		MigrationHost:      "10.0.0.1",
		Status:             status,
		LastHeartbeatAt:    hb,
		DrainTaskID:        drainTaskID,
		CreatedAt:          time.Now().UTC(),
		UpdatedAt:          time.Now().UTC(),
	}
	if err := cli.PutJSON(context.Background(), etcd.Key("nodes", id.String()), n); err != nil {
		t.Fatalf("seed node %q: %v", name, err)
	}
	return id
}

// etcdPutter is the narrow client surface seedNodeRow needs.
type etcdPutter interface {
	PutJSON(ctx context.Context, key string, v any) error
}

// TestMarkUnreachableSkipsDrainingNode pins the heartbeat-reconcile seam against
// an active drain. MarkNodesUnreachable / MarkNodesGone select from a snapshot
// (liveNodes) and previously blind-put the new status, so a drain that flipped a
// node between the snapshot and the write would have its drain_task_id silently
// dropped - stranding the saga. The race is exercised at the per-node guard: a
// candidate node that the selection predicate accepts (ready/unreachable, stale
// heartbeat) but that already carries an active drain_task_id must be SKIPPED,
// left in its current status with the pointer intact, rather than demoted. The
// next sweep retries it once the drain finishes.
func TestMarkUnreachableSkipsDrainingNode(t *testing.T) {
	s, cli := startStore(t)
	ctx := context.Background()
	old := time.Now().Add(-10 * time.Minute)

	t.Run("unreachable", func(t *testing.T) {
		taskID := uuid.New()
		// A ready node with a stale heartbeat is a MarkNodesUnreachable candidate;
		// the drain_task_id models a drain flip that landed after the snapshot.
		id := seedNodeRow(t, cli, "mark-unreach-drain", store.NodeStatusReady, &old, &taskID)

		rows, err := s.MarkNodesUnreachable(ctx, time.Now().UTC())
		if err != nil {
			t.Fatalf("MarkNodesUnreachable: %v", err)
		}
		for _, r := range rows {
			if r.ID == id {
				t.Errorf("draining node %v was marked unreachable, want skipped", id)
			}
		}
		got, err := s.NodeByID(ctx, id)
		if err != nil {
			t.Fatalf("NodeByID: %v", err)
		}
		if got.Status != store.NodeStatusReady {
			t.Errorf("node status = %v, want ready (drain must not be clobbered)", got.Status)
		}
		if got.DrainTaskID == nil || *got.DrainTaskID != taskID {
			t.Errorf("node DrainTaskID = %v, want %v (drain pointer must survive)", got.DrainTaskID, taskID)
		}
	})

	t.Run("gone", func(t *testing.T) {
		taskID := uuid.New()
		// An unreachable node with a stale heartbeat is a MarkNodesGone candidate.
		id := seedNodeRow(t, cli, "mark-gone-drain", store.NodeStatusUnreachable, &old, &taskID)

		rows, err := s.MarkNodesGone(ctx, time.Now().Add(-5*time.Minute))
		if err != nil {
			t.Fatalf("MarkNodesGone: %v", err)
		}
		for _, r := range rows {
			if r.ID == id {
				t.Errorf("draining node %v was marked gone, want skipped", id)
			}
		}
		got, err := s.NodeByID(ctx, id)
		if err != nil {
			t.Fatalf("NodeByID: %v", err)
		}
		if got.Status != store.NodeStatusUnreachable {
			t.Errorf("node status = %v, want unreachable (drain must not be clobbered)", got.Status)
		}
		if got.DrainTaskID == nil || *got.DrainTaskID != taskID {
			t.Errorf("node DrainTaskID = %v, want %v (drain pointer must survive)", got.DrainTaskID, taskID)
		}
	})
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
