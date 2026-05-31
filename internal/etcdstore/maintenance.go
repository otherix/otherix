// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
