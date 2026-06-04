// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Periodic-worker reads and sweeps that operate over whole collections: task
// retention, node-health reconciliation, and the scan-trigger's pool list. Each
// is a bounded primary-prefix scan with in-app predicates that the periodic
// workers drive.

// maxTxnOps caps how many delete operations ride in a single transaction; etcd's
// default --max-txn-ops is 128, so retention sweeps commit in chunks below it.
const maxTxnOps = 120

// DeleteExpiredTasks removes finalized tasks past their per-state retention
// window (success past CompletedCutoff; failed / cancelled past FailedCutoff),
// dropping each task's created-by index alongside the row. Returns the number of
// tasks deleted.
func (s *Store) DeleteExpiredTasks(ctx context.Context, arg store.DeleteExpiredTasksParams) (int64, error) {
	items, err := s.c.Range(ctx, taskPrefix())
	if err != nil {
		return 0, err
	}
	var (
		ops     []clientv3.Op
		deleted int64
	)
	for _, kv := range items {
		var t store.Task
		if err := json.Unmarshal(kv.Value, &t); err != nil {
			return 0, fmt.Errorf("unmarshal task %q: %v", kv.Key, err)
		}
		if !taskRetentionExpired(t, arg) {
			continue
		}
		ops = append(ops, clientv3.OpDelete(taskKey(t.ID)))
		if t.CreatedBy != nil {
			ops = append(ops, clientv3.OpDelete(etcd.Key("index", "tasks", "created_by", t.CreatedBy.String(), t.ID.String())))
		}
		deleted++
	}
	if err := s.commitInChunks(ctx, ops); err != nil {
		return 0, fmt.Errorf("delete expired tasks: %v", err)
	}
	return deleted, nil
}

// taskRetentionExpired reports whether a finalized task is past its per-state
// retention cutoff.
func taskRetentionExpired(t store.Task, arg store.DeleteExpiredTasksParams) bool {
	if t.FinishedAt == nil {
		return false
	}
	switch t.Status {
	case store.TaskStatusSuccess:
		return t.FinishedAt.Before(arg.CompletedCutoff)
	case store.TaskStatusFailed, store.TaskStatusCancelled:
		return t.FinishedAt.Before(arg.FailedCutoff)
	default:
		return false
	}
}

// commitInChunks commits the operations in transactions of at most maxTxnOps so
// a large sweep never exceeds etcd's per-transaction op limit.
func (s *Store) commitInChunks(ctx context.Context, ops []clientv3.Op) error {
	for i := 0; i < len(ops); i += maxTxnOps {
		end := min(i+maxTxnOps, len(ops))
		if _, err := s.c.Raw().Txn(ctx).Then(ops[i:end]...).Commit(); err != nil {
			return err
		}
	}
	return nil
}

// FailedJobRetention is how long a failed job row is kept for debugging before
// jobs.cleanup sweeps it. The job's task row already carries the terminal
// status, so the job row is debug-only past this window.
const FailedJobRetention = 7 * 24 * time.Hour

// DeleteFailedJobs removes failed job rows whose FailedAt is older than
// olderThan. It never touches running jobs: redelivery of a crash-orphaned
// running job is a separate concern, and deleting one would abandon in-flight
// work. Returns the number of rows deleted.
func (s *Store) DeleteFailedJobs(ctx context.Context, olderThan time.Time) (int64, error) {
	items, err := s.c.Range(ctx, jobsPrefix())
	if err != nil {
		return 0, err
	}
	var (
		ops     []clientv3.Op
		deleted int64
	)
	for _, kv := range items {
		var j Job
		if err := json.Unmarshal(kv.Value, &j); err != nil {
			return 0, fmt.Errorf("unmarshal job %q: %v", kv.Key, err)
		}
		if j.State != JobStateFailed || j.FailedAt == nil || !j.FailedAt.Before(olderThan) {
			continue
		}
		ops = append(ops, clientv3.OpDelete(jobKey(j.ID)))
		deleted++
	}
	if err := s.commitInChunks(ctx, ops); err != nil {
		return 0, fmt.Errorf("delete failed jobs: %v", err)
	}
	return deleted, nil
}

// JobsCleanupFunc returns a periodic function that sweeps failed job rows older
// than FailedJobRetention. The scheduler logs any returned error, so this only
// logs the swept count.
func JobsCleanupFunc(st *Store, log *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		n, err := st.DeleteFailedJobs(ctx, time.Now().UTC().Add(-FailedJobRetention))
		if err != nil {
			return fmt.Errorf("delete failed jobs: %v", err)
		}
		if n > 0 {
			log.InfoContext(ctx, "jobs.cleanup", "deleted", n)
		}
		return nil
	}
}

// DeleteOrphanedNetworkNodeStatus removes every network_node_status record whose
// network id no longer resolves to a live (non-deleted) network, returning the
// number of records deleted. It backstops purgeNetworkNodeStatus, which runs
// best-effort after a network soft-delete (DeleteNetwork) and can leave records
// behind on a partial commit - nothing else re-drives that cleanup. The deletes
// commit in chunks under etcd's per-transaction op limit.
func (s *Store) DeleteOrphanedNetworkNodeStatus(ctx context.Context) (int64, error) {
	items, err := s.c.Range(ctx, networkNodeStatusPrefix)
	if err != nil {
		return 0, err
	}
	live := make(map[uuid.UUID]bool)
	var (
		ops     []clientv3.Op
		deleted int64
	)
	for _, kv := range items {
		var st store.NetworkNodeStatus
		if err := json.Unmarshal(kv.Value, &st); err != nil {
			return 0, fmt.Errorf("unmarshal network_node_status %q: %v", kv.Key, err)
		}
		ok, seen := live[st.NetworkID]
		if !seen {
			if _, nerr := s.NetworkByID(ctx, st.NetworkID); nerr != nil {
				if !errors.Is(nerr, store.ErrNotFound) {
					return 0, nerr
				}
				ok = false
			} else {
				ok = true
			}
			live[st.NetworkID] = ok
		}
		if ok {
			continue
		}
		ops = append(ops, clientv3.OpDelete(kv.Key))
		deleted++
	}
	if err := s.commitInChunks(ctx, ops); err != nil {
		return 0, fmt.Errorf("delete orphaned network_node_status: %v", err)
	}
	return deleted, nil
}

// PromoteHealthyNodes flips nodes in 'pending' or 'unreachable' with a heartbeat
// at or after freshAfter to 'ready', returning the affected rows. 'cordoned' and
// 'draining' are operator-pinned and untouched.
func (s *Store) PromoteHealthyNodes(ctx context.Context, freshAfter time.Time) ([]store.PromoteHealthyNodesRow, error) {
	nodes, err := s.liveNodes(ctx)
	if err != nil {
		return nil, err
	}
	var rows []store.PromoteHealthyNodesRow
	now := time.Now().UTC()
	for _, n := range nodes {
		if n.Status != store.NodeStatusPending && n.Status != store.NodeStatusUnreachable {
			continue
		}
		if n.LastHeartbeatAt == nil || n.LastHeartbeatAt.Before(freshAfter) {
			continue
		}
		n.Status = store.NodeStatusReady
		n.UpdatedAt = now
		if err := s.c.PutJSON(ctx, nodeKey(n.ID), n); err != nil {
			return nil, err
		}
		rows = append(rows, store.PromoteHealthyNodesRow{ID: n.ID, Name: n.Name, Status: n.Status})
	}
	return rows, nil
}

// MarkNodesUnreachable demotes nodes in 'ready' or 'pending' whose heartbeat is
// missing or older than staleBefore to 'unreachable', returning the affected
// rows. 'cordoned' and 'draining' are operator-pinned and untouched.
func (s *Store) MarkNodesUnreachable(ctx context.Context, staleBefore time.Time) ([]store.MarkNodesUnreachableRow, error) {
	nodes, err := s.liveNodes(ctx)
	if err != nil {
		return nil, err
	}
	var rows []store.MarkNodesUnreachableRow
	now := time.Now().UTC()
	for _, n := range nodes {
		if n.Status != store.NodeStatusReady && n.Status != store.NodeStatusPending {
			continue
		}
		if n.LastHeartbeatAt != nil && !n.LastHeartbeatAt.Before(staleBefore) {
			continue
		}
		n.Status = store.NodeStatusUnreachable
		n.UpdatedAt = now
		if err := s.c.PutJSON(ctx, nodeKey(n.ID), n); err != nil {
			return nil, err
		}
		rows = append(rows, store.MarkNodesUnreachableRow{ID: n.ID, Name: n.Name, LastHeartbeatAt: n.LastHeartbeatAt})
	}
	return rows, nil
}

// MarkNodesGone advances nodes already in 'unreachable' whose heartbeat is
// missing or older than goneBefore to the terminal 'gone' status, returning the
// affected rows. It deliberately does NOT orphan the node's vm_runtime: the
// datapath (per-VM FDB + the WireGuard mesh) converges via the gone-liveness
// guards, while leaving current_node_id intact avoids a split-brain if a long
// network partition heals (the node's qemu may still be running). 'gone' is
// terminal - recovery is an explicit operator action. 'ready'/'pending'/
// 'cordoned'/'draining' are untouched.
func (s *Store) MarkNodesGone(ctx context.Context, goneBefore time.Time) ([]store.MarkNodesGoneRow, error) {
	nodes, err := s.liveNodes(ctx)
	if err != nil {
		return nil, err
	}
	var rows []store.MarkNodesGoneRow
	now := time.Now().UTC()
	for _, n := range nodes {
		if n.Status != store.NodeStatusUnreachable {
			continue
		}
		if n.LastHeartbeatAt != nil && !n.LastHeartbeatAt.Before(goneBefore) {
			continue
		}
		n.Status = store.NodeStatusGone
		n.UpdatedAt = now
		if err := s.c.PutJSON(ctx, nodeKey(n.ID), n); err != nil {
			return nil, err
		}
		rows = append(rows, store.MarkNodesGoneRow{ID: n.ID, Name: n.Name})
	}
	return rows, nil
}

// liveNodes loads every non-deleted node row.
func (s *Store) liveNodes(ctx context.Context) ([]store.Node, error) {
	items, err := s.c.Range(ctx, nodePrefix())
	if err != nil {
		return nil, err
	}
	out := make([]store.Node, 0, len(items))
	for _, kv := range items {
		var n store.Node
		if err := json.Unmarshal(kv.Value, &n); err != nil {
			return nil, fmt.Errorf("unmarshal node %q: %v", kv.Key, err)
		}
		if n.DeletedAt != nil {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// ListPoolsNeedingScan returns active pools on a scannable node ('pending',
// 'ready', or 'cordoned') that have no in-flight scan task, ordered by pool id.
// Drives the periodic scan trigger.
func (s *Store) ListPoolsNeedingScan(ctx context.Context) ([]store.ListPoolsNeedingScanRow, error) {
	inflight, err := s.poolsWithInflightScan(ctx)
	if err != nil {
		return nil, err
	}
	items, err := s.c.Range(ctx, storagePoolPrefix())
	if err != nil {
		return nil, err
	}
	var rows []store.ListPoolsNeedingScanRow
	for _, kv := range items {
		var p store.StoragePool
		if err := json.Unmarshal(kv.Value, &p); err != nil {
			return nil, fmt.Errorf("unmarshal storage pool %q: %v", kv.Key, err)
		}
		if p.DeletedAt != nil || inflight[p.ID] {
			continue
		}
		node, err := s.NodeByID(ctx, p.NodeID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		if !nodeScannable(node.Status) {
			continue
		}
		rows = append(rows, store.ListPoolsNeedingScanRow{ID: p.ID, Name: p.Name, NodeID: p.NodeID})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID.String() < rows[j].ID.String() })
	return rows, nil
}

// poolsWithInflightScan returns the set of pool ids referenced by a pending or
// running storage_pool.scan task.
func (s *Store) poolsWithInflightScan(ctx context.Context) (map[uuid.UUID]bool, error) {
	items, err := s.c.Range(ctx, taskPrefix())
	if err != nil {
		return nil, err
	}
	inflight := make(map[uuid.UUID]bool)
	for _, kv := range items {
		var t store.Task
		if err := json.Unmarshal(kv.Value, &t); err != nil {
			return nil, fmt.Errorf("unmarshal task %q: %v", kv.Key, err)
		}
		if t.Type != "storage_pool.scan" {
			continue
		}
		if t.Status != store.TaskStatusPending && t.Status != store.TaskStatusRunning {
			continue
		}
		if t.ResourceID != nil {
			inflight[*t.ResourceID] = true
		}
	}
	return inflight, nil
}

// nodeScannable reports whether a node status admits a storage-pool scan.
func nodeScannable(status store.NodeStatus) bool {
	switch status {
	case store.NodeStatusPending, store.NodeStatusReady, store.NodeStatusCordoned:
		return true
	default:
		return false
	}
}
