// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// Nodes are a bounded, cluster-wide collection addressed by UUID with a
// case-insensitive name guard (uq_nodes_name) that doubles as the NodeByName
// index. The status/cordoned_at columns live on the row itself (no separate
// node_status table). NodeEffectiveByID / ListNodesEffective project the SQL
// node_effective_availability view: effective CPU/memory is raw availability
// minus pending VM reservations (vms pinned to the node, created after the last
// heartbeat). DeleteNode mirrors the force-cascade: cancel active migrations +
// orphan vm_runtime, then soft-delete.

func nodeKey(id uuid.UUID) string { return etcd.Key("nodes", id.String()) }

func nodePrefix() string { return etcd.Key("nodes") + "/" }

func nodeNameGuard(name string) string {
	return etcd.Key("uniq", "nodes", "name", strings.ToLower(name))
}

// vmKey, vmRuntimeKey, and migrationKey are the canonical primary keys for the
// VM-domain resources. They are defined here because DeleteNode's effective-
// availability and force-cascade read them; the vms / migrations slices reuse
// these same helpers (one definition per package).
func vmKey(id uuid.UUID) string { return etcd.Key("vms", id.String()) }

func vmRuntimeKey(vmID uuid.UUID) string { return etcd.Key("vm_runtime", vmID.String()) }

func migrationKey(id uuid.UUID) string { return etcd.Key("migrations", id.String()) }

// vmsPinnedNodeIndexPrefix lists the vms pinned to a node (maintained by the vms
// slice) - consumed by the effective-availability pending term.
func vmsPinnedNodeIndexPrefix(nodeID uuid.UUID) string {
	return etcd.Key("index", "vms", "pinned_node", nodeID.String()) + "/"
}

// vmRuntimeNodeIndexPrefix lists the vm_runtime rows currently bound to a node
// (maintained by the vms slice) - consumed by DeleteNode's vm count + orphan.
func vmRuntimeNodeIndexPrefix(nodeID uuid.UUID) string {
	return etcd.Key("index", "vm_runtime", "node", nodeID.String()) + "/"
}

// migrationsNodeIndexPrefix lists every migration touching a node as source or
// target (maintained by the migrations slice) - consumed by DeleteNode's active
// migration count + cancel, which filter to non-terminal phases by reading the
// primary.
func migrationsNodeIndexPrefix(nodeID uuid.UUID) string {
	return etcd.Key("index", "migrations", "node", nodeID.String()) + "/"
}

// NodeByID returns the bare node row, or store.ErrNotFound (soft-deleted rows
// are invisible).
func (s *Store) NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error) {
	return s.nodeByIDAtRev(ctx, id, 0)
}

// nodeByIDAtRev is NodeByID pinned to an MVCC revision (rev==0 reads latest).
// The FDB projection's gone-resolver reads it at the projection's snapshot
// revision so a node soft-delete or status flip mid-join cannot tear the read.
func (s *Store) nodeByIDAtRev(ctx context.Context, id uuid.UUID, rev int64) (store.Node, error) {
	var n store.Node
	found, err := s.c.GetJSONAtRev(ctx, nodeKey(id), rev, &n)
	if err != nil {
		return store.Node{}, err
	}
	if !found || n.DeletedAt != nil {
		return store.Node{}, store.ErrNotFound
	}
	return n, nil
}

// NodeByName returns the node with the given name (case-insensitive) via the
// name guard, or store.ErrNotFound.
func (s *Store) NodeByName(ctx context.Context, name string) (store.Node, error) {
	id, found, err := s.resolveGuard(ctx, nodeNameGuard(name))
	if err != nil {
		return store.Node{}, err
	}
	if !found {
		return store.Node{}, store.ErrNotFound
	}
	return s.NodeByID(ctx, id)
}

// AllNodes returns every non-deleted node row. The durability reconcile loop and
// the snapshot durability projection use it to compute the live-node set and to
// expand an artifact pool's membership by name. Soft-deleted rows are excluded;
// status (including unreachable/gone) is preserved on the returned rows.
func (s *Store) AllNodes(ctx context.Context) ([]store.Node, error) {
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

// NodeEffectiveByID returns the node joined with its effective availability, or
// store.ErrNotFound.
func (s *Store) NodeEffectiveByID(ctx context.Context, id uuid.UUID) (store.NodeEffectiveAvailability, error) {
	n, err := s.NodeByID(ctx, id)
	if err != nil {
		return store.NodeEffectiveAvailability{}, err
	}
	return s.nodeEffective(ctx, n)
}

// CreateNode inserts a node, stamping created_at/updated_at, writing the name
// guard + primary atomically. A name collision returns store.ErrNodeNameExists.
func (s *Store) CreateNode(ctx context.Context, arg store.CreateNodeParams) (store.Node, error) {
	now := time.Now().UTC()
	n := store.Node{
		ID:                        arg.ID,
		Name:                      arg.Name,
		GatewayRole:               arg.Gateway,
		Architecture:              arg.Architecture,
		AdvertisedEndpoint:        arg.AdvertisedEndpoint,
		IngressAdvertisedEndpoint: arg.IngressAdvertisedEndpoint,
		MigrationHost:             arg.MigrationHost,
		MigrationPortRangeStart:   arg.MigrationPortRangeStart,
		MigrationPortRangeEnd:     arg.MigrationPortRangeEnd,
		Status:                    arg.Status,
		CreatedAt:                 now,
		UpdatedAt:                 now,
	}
	val, err := etcd.Marshal(n)
	if err != nil {
		return store.Node{}, err
	}
	guard := nodeNameGuard(n.Name)
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(
			clientv3.OpPut(guard, n.ID.String()),
			clientv3.OpPut(nodeKey(n.ID), string(val)),
		).
		Commit()
	if err != nil {
		return store.Node{}, fmt.Errorf("create node txn: %v", err)
	}
	if !resp.Succeeded {
		return store.Node{}, store.ErrNodeNameExists
	}
	return n, nil
}

// CordonNode marks the node cordoned, stamping cordoned_at and bumping
// updated_at. The handler gates valid transitions before calling.
func (s *Store) CordonNode(ctx context.Context, id uuid.UUID) (store.Node, error) {
	return s.setNodeCordon(ctx, id, store.NodeStatusCordoned, true)
}

// UncordonNode returns the node to ready, clearing cordoned_at. The handler
// gates valid transitions before calling.
func (s *Store) UncordonNode(ctx context.Context, id uuid.UUID) (store.Node, error) {
	return s.setNodeCordon(ctx, id, store.NodeStatusReady, false)
}

// setNodeCordon applies a cordon/uncordon status change, setting/clearing
// cordoned_at and bumping updated_at.
//
// The handler validates the source status (rejecting draining) before calling,
// but that check is a TOCTOU: a drain can flip the node between the handler's
// read and this write. Two guards close that window, both on the fresh re-read.
// First, the node is refused if it now carries an active drain (DrainTaskID set)
// - overwriting a live drain would drop its drain_task_id and strand the saga.
// Second, the write commits under a ModRevision CAS on the row, so any concurrent
// mutation between this read and the put loses. Either guard tripping returns
// store.ErrConcurrentUpdate; the operator retries.
func (s *Store) setNodeCordon(ctx context.Context, id uuid.UUID, status store.NodeStatus, cordon bool) (store.Node, error) {
	n, modRev, err := s.nodeWithRev(ctx, id)
	if err != nil {
		return store.Node{}, err
	}
	if n.DrainTaskID != nil {
		// A drain won the race and owns the node; refuse rather than clobber its
		// drain_task_id and strand the saga.
		return store.Node{}, store.ErrConcurrentUpdate
	}
	n.Status = status
	now := time.Now().UTC()
	if cordon {
		n.CordonedAt = &now
	} else {
		n.CordonedAt = nil
	}
	n.UpdatedAt = now
	val, err := etcd.Marshal(n)
	if err != nil {
		return store.Node{}, err
	}
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(nodeKey(id)), "=", modRev)).
		Then(clientv3.OpPut(nodeKey(id), string(val))).
		Commit()
	if err != nil {
		return store.Node{}, fmt.Errorf("set node cordon txn: %v", err)
	}
	if !resp.Succeeded {
		return store.Node{}, store.ErrConcurrentUpdate
	}
	return n, nil
}

// SetNodeGatewayRole assigns or clears the gateway role on a node under a
// ModRevision compare-and-set. It is the ONLY writer of GatewayRole outside
// create/join and the node-delete cascade; the whole-row heartbeat writers are
// made CAS-safe separately so a heartbeat cannot clobber a concurrent toggle.
// Enabling an already-enabled node (or disabling an already-disabled one) is a
// no-op that returns the current row. Returns store.ErrConcurrentUpdate if the
// row changed under it, store.ErrNotFound for a missing or soft-deleted node.
func (s *Store) SetNodeGatewayRole(ctx context.Context, id uuid.UUID, enabled bool) (store.Node, error) {
	n, modRev, err := s.nodeWithRev(ctx, id)
	if err != nil {
		return store.Node{}, err
	}
	if n.GatewayRole == enabled {
		return n, nil
	}
	n.GatewayRole = enabled
	n.UpdatedAt = time.Now().UTC()
	val, err := etcd.Marshal(n)
	if err != nil {
		return store.Node{}, err
	}
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(nodeKey(id)), "=", modRev)).
		Then(clientv3.OpPut(nodeKey(id), string(val))).
		Commit()
	if err != nil {
		return store.Node{}, fmt.Errorf("set node gateway role txn: %v", err)
	}
	if !resp.Succeeded {
		return store.Node{}, store.ErrConcurrentUpdate
	}
	return n, nil
}

// nodeWithRev reads a node row and the ModRevision its primary key was last
// written at, so a caller can CAS-guard a multi-key write against a concurrent
// mutation of the row. Soft-deleted rows are reported as store.ErrNotFound,
// matching NodeByID. Modeled on the raw-Get + ModRevision read loadCutoverState
// uses; the bare NodeByID path cannot return the revision the CAS needs.
func (s *Store) nodeWithRev(ctx context.Context, id uuid.UUID) (store.Node, int64, error) {
	resp, err := s.c.Raw().Get(ctx, nodeKey(id))
	if err != nil {
		return store.Node{}, 0, fmt.Errorf("get node %s: %v", id, err)
	}
	if len(resp.Kvs) == 0 {
		return store.Node{}, 0, store.ErrNotFound
	}
	var n store.Node
	if err := json.Unmarshal(resp.Kvs[0].Value, &n); err != nil {
		return store.Node{}, 0, fmt.Errorf("unmarshal node %s: %v", id, err)
	}
	if n.DeletedAt != nil {
		return store.Node{}, 0, store.ErrNotFound
	}
	return n, resp.Kvs[0].ModRevision, nil
}

// StartNodeDrain flips a ready or cordoned node to draining and enqueues the
// drain task plus its backing job in one transaction, stamping the new task's
// drain_task_id onto the node. A node in any other status returns
// store.ErrNodeNotDrainable. The whole write is CAS-guarded on the node's
// ModRevision, so a concurrent node mutation (cordon, heartbeat, delete) loses
// and returns store.ErrConcurrentUpdate.
func (s *Store) StartNodeDrain(ctx context.Context, nodeID uuid.UUID, taskParams store.CreateTaskParams, args queue.JobArgs) (store.Task, error) {
	node, modRev, err := s.nodeWithRev(ctx, nodeID)
	if err != nil {
		return store.Task{}, err
	}
	if node.Status != store.NodeStatusReady && node.Status != store.NodeStatusCordoned {
		return store.Task{}, store.ErrNodeNotDrainable
	}
	now := time.Now().UTC()
	node.Status = store.NodeStatusDraining
	node.DrainTaskID = &taskParams.ID
	node.UpdatedAt = now
	nodeVal, err := etcd.Marshal(node)
	if err != nil {
		return store.Task{}, err
	}
	seq, jobOp, err := s.enqueueJobOp(ctx, args)
	if err != nil {
		return store.Task{}, err
	}
	task := taskFromParams(taskParams, seq)
	taskVal, err := etcd.Marshal(task)
	if err != nil {
		return store.Task{}, err
	}
	ops := []clientv3.Op{
		clientv3.OpPut(nodeKey(node.ID), string(nodeVal)),
		clientv3.OpPut(taskKey(task.ID), string(taskVal)),
		jobOp,
	}
	ops = append(ops, taskIndexOps(task)...)
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(nodeKey(node.ID)), "=", modRev)).
		Then(ops...).
		Commit()
	if err != nil {
		return store.Task{}, fmt.Errorf("start node drain txn: %v", err)
	}
	if !resp.Succeeded {
		return store.Task{}, store.ErrConcurrentUpdate
	}
	return task, nil
}

// FinishNodeDrain finalizes a drain: it stamps the task's terminal status and
// result, deletes the cooperative cancel marker, and, when the node is still
// draining, flips it back to cordoned and clears drain_task_id. It is
// idempotent - a redelivery whose task already reached a terminal status is a
// no-op (returns nil). When the txn writes the node row it commits under a
// ModRevision CAS on that row; a concurrent node mutation returns
// store.ErrConcurrentUpdate. When no node write is needed the txn carries no
// node compare, so a concurrent heartbeat blind-put on the node key does not
// spuriously conflict.
//
// The task row is read-then-written without a CAS guard. That is safe only
// because a drain task has a single writer in practice: the dispatcher grants
// one delivery ownership at a time and the drain saga owns the task, so no other
// writer races the task put. A future caller must not assume the txn CAS guards
// the task row - it does not.
func (s *Store) FinishNodeDrain(ctx context.Context, nodeID, taskID uuid.UUID, status store.TaskStatus, result []byte) error {
	node, modRev, err := s.nodeWithRev(ctx, nodeID)
	if err != nil {
		return err
	}
	task, err := s.TaskByID(ctx, taskID)
	if err != nil {
		return err
	}
	if isTerminalTaskStatus(task.Status) {
		return nil
	}
	now := time.Now().UTC()
	task.Status = status
	task.Result = result
	task.FinishedAt = &now
	taskVal, err := etcd.Marshal(task)
	if err != nil {
		return err
	}
	ops := []clientv3.Op{
		clientv3.OpPut(taskKey(task.ID), string(taskVal)),
		clientv3.OpDelete(drainCancelKey(taskID)),
	}
	// The node ModRevision compare is added to the txn If only when the txn
	// actually writes the node row (branches A and B). When no node write is
	// needed (branch C) the txn runs unconditional - a frequent heartbeat
	// blind-put on the node key would otherwise spuriously fail the CAS even
	// though the node is not being touched.
	var cmps []clientv3.Cmp
	writeNode := func() error {
		node.UpdatedAt = now
		nodeVal, merr := etcd.Marshal(node)
		if merr != nil {
			return merr
		}
		ops = append(ops, clientv3.OpPut(nodeKey(node.ID), string(nodeVal)))
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(nodeKey(node.ID)), "=", modRev))
		return nil
	}
	switch {
	case node.Status == store.NodeStatusDraining:
		node.Status = store.NodeStatusCordoned
		node.DrainTaskID = nil
		if err := writeNode(); err != nil {
			return err
		}
	case node.DrainTaskID != nil:
		node.DrainTaskID = nil
		if err := writeNode(); err != nil {
			return err
		}
	}
	resp, err := s.c.Raw().Txn(ctx).
		If(cmps...).
		Then(ops...).
		Commit()
	if err != nil {
		return fmt.Errorf("finish node drain txn: %v", err)
	}
	if !resp.Succeeded {
		return store.ErrConcurrentUpdate
	}
	return nil
}

// drainCancelKey is the cooperative cancel marker for an in-flight drain task.
// Its presence asks the drain saga to stop scheduling further evictions; the
// marker is deleted when the drain finalizes.
func drainCancelKey(taskID uuid.UUID) string {
	return etcd.Key("node_drain_cancel", taskID.String())
}

// RequestDrainCancel sets the cooperative cancel marker for the drain task. It
// is best-effort signalling, not a hard stop: the saga observes it between
// eviction steps.
//
// The marker is normally deleted at finalize (FinishNodeDrain, or
// DeleteDrainCancel on the task-only finalize path). The etcd wrapper exposes no
// lease/TTL put, so the marker is not self-expiring; a finalize path that never
// runs would leak it. That leak is harmless: the key is content-free and scoped
// by the task's fresh UUID, so a stale marker can never match a future drain.
func (s *Store) RequestDrainCancel(ctx context.Context, taskID uuid.UUID) error {
	return s.c.Put(ctx, drainCancelKey(taskID), []byte("1"))
}

// DeleteDrainCancel removes the cooperative cancel marker for the drain task.
// It is idempotent (a missing marker is a no-op) and is called on finalize paths
// that do not go through FinishNodeDrain (e.g. a node force-deleted mid-drain,
// which finalizes the task only) so the marker does not leak.
func (s *Store) DeleteDrainCancel(ctx context.Context, taskID uuid.UUID) error {
	if _, err := s.c.Delete(ctx, drainCancelKey(taskID)); err != nil {
		return fmt.Errorf("delete drain cancel marker: %v", err)
	}
	return nil
}

// DrainCancelRequested reports whether a cooperative cancel has been requested
// for the drain task.
func (s *Store) DrainCancelRequested(ctx context.Context, taskID uuid.UUID) (bool, error) {
	_, found, err := s.c.Get(ctx, drainCancelKey(taskID))
	if err != nil {
		return false, err
	}
	return found, nil
}

// listNodesByStatus returns every non-deleted node row in the given status.
func (s *Store) listNodesByStatus(ctx context.Context, status store.NodeStatus) ([]store.Node, error) {
	nodes, err := s.liveNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.Node, 0, len(nodes))
	for _, n := range nodes {
		if n.Status == status {
			out = append(out, n)
		}
	}
	return out, nil
}

// CountDrainingNodes returns how many non-deleted nodes are currently in the
// draining status. A node in draining IS an active drain, so the drain handler
// uses this as the admission counter that caps concurrent drains: each drain
// holds a worker slot for its whole duration plus its migrate-job slots, so an
// uncapped fleet drain would exhaust the bounded worker pool.
func (s *Store) CountDrainingNodes(ctx context.Context) (int, error) {
	nodes, err := s.listNodesByStatus(ctx, store.NodeStatusDraining)
	if err != nil {
		return 0, err
	}
	return len(nodes), nil
}

// ReconcileStuckDrain finds nodes wedged in draining whose drain is no longer
// progressing and finalizes them to cordoned, clearing drain_task_id. Returns
// the count fixed. Non-destructive: cordoned is the normal drain terminal.
//
// A draining node is left alone only when a LIVE drain owns it. The task status
// alone is not a sufficient liveness signal: a drain whose backing job exhausts
// its attempt budget leaves the task stuck in running while the dispatcher marks
// only the job failed, so a task-status-only check would treat that wedge as a
// live drain forever. The real liveness signal is the backing job. For a node
// whose task is still pending/running, the job is read through task.JobID:
//   - job pending or running -> a live saga owns it -> skip.
//   - job absent (deleted) or terminal (completed/failed) -> the saga is dead ->
//     finalize, exactly as for a missing or terminal task.
//
// A nil task.JobID (defensive; EnqueueTask always stamps one) is treated as dead.
func (s *Store) ReconcileStuckDrain(ctx context.Context, reconciledResult []byte) (int, error) {
	nodes, err := s.listNodesByStatus(ctx, store.NodeStatusDraining)
	if err != nil {
		return 0, err
	}
	fixed := 0
	for _, n := range nodes {
		live, lerr := s.drainStillLive(ctx, n)
		if lerr != nil {
			return fixed, lerr
		}
		if live {
			continue // a live drain is in progress; leave it
		}
		// Finalize a wedged non-terminal drain task BEFORE cordoning. cordonStuckDrain
		// clears drain_task_id and flips the node out of draining, so the backstop
		// never revisits it; a task left running would then never reach a terminal
		// state (retention reaps by finished_at) and a cancel against it would be a
		// dead letter. Finalizing first is crash-safe: a crash between the task
		// finalize and the cordon leaves the node draining, so the next pass re-runs
		// (the now-terminal task is skipped by drainStillLive) and cordons.
		if n.DrainTaskID != nil {
			if ferr := s.finalizeStuckDrainTask(ctx, *n.DrainTaskID, reconciledResult); ferr != nil {
				return fixed, ferr
			}
		}
		cordoned, cerr := s.cordonStuckDrain(ctx, n.ID)
		if cerr != nil {
			return fixed, cerr
		}
		if cordoned {
			fixed++
		}
	}
	return fixed, nil
}

// drainStillLive reports whether a draining node is owned by a live drain (its
// task is pending/running AND its backing job is still alive). A torn node (nil
// drain_task_id), a missing/terminal task, or a non-terminal task whose backing
// job is dead all report not-live - the backstop must cordon them.
func (s *Store) drainStillLive(ctx context.Context, n store.Node) (bool, error) {
	if n.DrainTaskID == nil {
		return false, nil // torn: draining with nothing driving it
	}
	t, err := s.TaskByID(ctx, *n.DrainTaskID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil // task reaped while the node stayed draining
	}
	if err != nil {
		return false, err
	}
	if t.Status != store.TaskStatusPending && t.Status != store.TaskStatusRunning {
		return false, nil // task terminal: the saga recorded its outcome
	}
	// Task non-terminal: the backing job is the real liveness signal. A drain whose
	// job exhausted its attempts leaves the task stuck running with only the job
	// marked failed, so a task-status-only check would never un-wedge it.
	return s.drainJobLive(ctx, t.JobID)
}

// cordonStuckDrain flips a wedged draining node back to cordoned and clears its
// drain_task_id under a ModRevision CAS, reporting whether the write committed.
// It re-reads the row and no-ops (false) when the node is no longer draining
// (raced) or lost the CAS, so a concurrent legitimate mutation always wins.
func (s *Store) cordonStuckDrain(ctx context.Context, id uuid.UUID) (bool, error) {
	node, modRev, err := s.nodeWithRev(ctx, id)
	if err != nil || node.Status != store.NodeStatusDraining {
		return false, nil
	}
	node.Status = store.NodeStatusCordoned
	node.DrainTaskID = nil
	node.UpdatedAt = time.Now().UTC()
	val, err := etcd.Marshal(node)
	if err != nil {
		return false, err
	}
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(nodeKey(node.ID)), "=", modRev)).
		Then(clientv3.OpPut(nodeKey(node.ID), string(val))).
		Commit()
	if err != nil {
		return false, fmt.Errorf("reconcile stuck drain txn: %v", err)
	}
	return resp.Succeeded, nil
}

// drainJobLive reports whether the drain task's backing job is still a live owner
// of the saga. A live job is pending (queued) or running (claimed); an absent job
// (deleted) or a terminal job (completed/failed) means the saga is dead and the
// drain is no longer progressing. A nil jobID is treated as dead (defensive: a
// drain task always carries a job ref, so a nil here is a torn task, not a live
// drain).
func (s *Store) drainJobLive(ctx context.Context, jobID *int64) (bool, error) {
	if jobID == nil {
		return false, nil
	}
	job, _, found, err := s.jobWithRev(ctx, *jobID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	return job.State == JobStatePending || job.State == JobStateRunning, nil
}

// finalizeStuckDrainTask finalizes a wedged drain task to failed under a
// ModRevision CAS, stamping finished_at and the caller-supplied reconciled
// result so retention (which keys on finished_at) reaps it and a cancel is no
// longer a dead letter. It fails toward inaction: a task that is missing
// (already reaped) or already terminal (its saga recorded an outcome) is left
// untouched, and a lost CAS (a concurrent writer moved the task) is a no-op.
// reconciledResult is opaque to the store - the drain handler owns its shape.
func (s *Store) finalizeStuckDrainTask(ctx context.Context, taskID uuid.UUID, reconciledResult []byte) error {
	t, modRev, err := s.taskWithRev(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) {
		return nil // task reaped while the node stayed draining
	}
	if err != nil {
		return err
	}
	if isTerminalTaskStatus(t.Status) {
		return nil // the saga already recorded a terminal outcome; do not clobber
	}
	t.Status = store.TaskStatusFailed
	t.Result = reconciledResult
	now := time.Now().UTC()
	t.FinishedAt = &now
	val, err := etcd.Marshal(t)
	if err != nil {
		return err
	}
	// A lost CAS (Succeeded false) means a concurrent writer moved the task since
	// our read; leave it (they win) - fail toward inaction. No retry: with a dead
	// backing job there is no live worker to race, so at most a peer reconcile pass
	// beat us to the same finalize.
	if _, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(taskKey(taskID)), "=", modRev)).
		Then(clientv3.OpPut(taskKey(taskID), string(val))).
		Commit(); err != nil {
		return fmt.Errorf("finalize stuck drain task txn: %v", err)
	}
	return nil
}

// nodeMatchesListFilters reports whether node n passes the optional
// architecture/status/role filters and the cursor lower bound. poolNodes is the
// pool-owning node-id set, consulted only when a role filter is set (nil
// otherwise) so the derived hypervisor role is sourced from pool ownership.
func nodeMatchesListFilters(n store.Node, arg store.ListNodesEffectiveParams, poolNodes map[uuid.UUID]struct{}) bool {
	if arg.Architecture != nil && n.Architecture != *arg.Architecture {
		return false
	}
	if arg.Status != nil && n.Status != *arg.Status {
		return false
	}
	if arg.Role != nil {
		_, ownsPool := poolNodes[n.ID]
		if !slices.Contains(store.EffectiveRoles(n.GatewayRole, ownsPool), *arg.Role) {
			return false
		}
	}
	return afterCursor(n.CreatedAt, n.ID, arg.CursorCreatedAt, arg.CursorID)
}

// ListNodesEffective returns nodes joined with their effective availability,
// matching the optional architecture/status/role filters, ordered by
// (created_at, id) ascending, after the cursor, capped at LimitCount.
func (s *Store) ListNodesEffective(ctx context.Context, arg store.ListNodesEffectiveParams) ([]store.NodeEffectiveAvailability, error) {
	items, err := s.c.Range(ctx, nodePrefix())
	if err != nil {
		return nil, err
	}
	var poolNodes map[uuid.UUID]struct{}
	if arg.Role != nil {
		poolNodes, err = s.NodeIDsWithPool(ctx)
		if err != nil {
			return nil, err
		}
	}
	out := make([]store.NodeEffectiveAvailability, 0, len(items))
	for _, kv := range items {
		var n store.Node
		if err := json.Unmarshal(kv.Value, &n); err != nil {
			return nil, fmt.Errorf("unmarshal node %q: %v", kv.Key, err)
		}
		if n.DeletedAt != nil {
			continue
		}
		if !nodeMatchesListFilters(n, arg, poolNodes) {
			continue
		}
		eff, err := s.nodeEffective(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, eff)
	}
	sortByCreatedAtID(out, func(e store.NodeEffectiveAvailability) (time.Time, uuid.UUID) {
		return e.CreatedAt, e.ID
	})
	if n := int(arg.LimitCount); n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// DeleteNode soft-deletes the node. Without force it refuses with
// *store.ResourceInUseError (keys "vms" + "active_migrations", both always
// present) when the node still hosts vm_runtime rows or active migrations. With
// force it cancels every active migration touching the node and orphans every
// vm_runtime row on it (clearing current_node_id, leaving vms.desired_phase
// untouched), recording the counts in the outcome. callerID is recorded in the
// migration cancel reason for audit. On any proceeding delete (force and
// non-force alike) it also revokes the node's agent certs so the deleted node
// can no longer authenticate over mTLS.
func (s *Store) DeleteNode(ctx context.Context, id uuid.UUID, force bool, callerID uuid.UUID) (store.NodeDeleteOutcome, error) {
	n, err := s.NodeByID(ctx, id)
	if err != nil {
		return store.NodeDeleteOutcome{}, err
	}

	// Set the node delete-intent FIRST so no new bind or migration cutover can pin a
	// VM here past this point (buildBindTxn + CommitMigrationCutover guard on it);
	// the VM set gated/evacuated below is then stable. The intent is cleared on every
	// non-finalizing exit (defer), and the node soft-delete finalize CASes on this
	// rev. See deleting_intent.go.
	intentKey := nodeDeletingKey(id)
	myRev, err := s.setDeleteIntent(ctx, intentKey, time.Now())
	if err != nil {
		return store.NodeDeleteOutcome{}, err
	}
	finalized := false
	defer func() {
		if !finalized {
			// Guarded on our rev: a no-op if a reaper/racing delete already severed it.
			s.clearDeleteIntent(ctx, intentKey, myRev)
		}
	}()

	// Gate on the UNION of observed (vm_runtime homed here) and declared (pinned
	// here, incl. pinned-but-unobserved) VMs, so a committed-but-unreported bind
	// still blocks a non-force delete and is evacuated by a force delete.
	union, err := s.unionVMIDsForNode(ctx, id)
	if err != nil {
		return store.NodeDeleteOutcome{}, err
	}
	activeMigs, err := s.activeMigrationsOnNode(ctx, id)
	if err != nil {
		return store.NodeDeleteOutcome{}, err
	}
	if !force && (len(union) > 0 || len(activeMigs) > 0) {
		return store.NodeDeleteOutcome{}, &store.ResourceInUseError{Resources: map[string]int64{
			"vms":               int64(len(union)),
			"active_migrations": int64(len(activeMigs)),
		}}
	}

	var out store.NodeDeleteOutcome
	if force {
		// Cancel the active migrations and evacuate the VMs through their own per-row
		// CAS (NOT as blind puts in the node cascade below): a concurrent
		// cutover/heartbeat that commits between our snapshot and this write must WIN,
		// so we never clobber a just-completed cutover. On CAS-retry exhaustion these
		// return an error and we abort the whole delete BEFORE the node soft-delete
		// (fail toward inaction; the operator retries).
		reason := fmt.Sprintf("source/target node %s force-deleted by user %s", id, callerID)
		out.MigrationsCancelled, err = s.cancelNodeMigrations(ctx, activeMigs, reason)
		if err != nil {
			return store.NodeDeleteOutcome{}, err
		}
		// Route each VM by runtime existence: observed -> orphan (keeps disk),
		// pinned-but-unobserved -> full bind-rollback (returns it to unscheduled).
		out.VMsOrphaned, out.VMsRolledBack, err = s.evacuateNodeVMs(ctx, id, union)
		if err != nil {
			return store.NodeDeleteOutcome{}, err
		}
	}

	certsRevoked, err := s.softDeleteNodeRow(ctx, n, intentKey, myRev)
	if err != nil {
		return store.NodeDeleteOutcome{}, err
	}
	out.CertsRevoked = certsRevoked
	finalized = true
	return out, nil
}

// softDeleteNodeRow assembles and commits the node-soft-delete cascade - the
// WireGuard fabric purge, the agent-cert revocation, the per-node leaf reap
// (gateway memberships + tenant-IP reservations + published-listener status), the
// name-guard delete, and the node put - under the delete-intent finalize CAS
// (commitCascadeWithNodeIntent). The leaf reap and cert revoke are ordered ahead
// of the node soft-delete (see nodeDeleteCascade) so a crash+retry can re-run them
// idempotently. Returns the number of agent certs revoked.
func (s *Store) softDeleteNodeRow(ctx context.Context, n store.Node, intentKey string, myRev int64) (int64, error) {
	id := n.ID
	wgRec, err := s.agentWireguardRecordForDelete(ctx, id)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	certOps, certsRevoked, err := s.revokeNodeAgentCertsOps(ctx, id, now, "node deleted")
	if err != nil {
		return 0, fmt.Errorf("revoke agent certs for node delete: %v", err)
	}
	reapOps, err := s.nodeDeleteReapOps(ctx, id)
	if err != nil {
		return 0, err
	}
	n.DeletedAt = &now
	n.UpdatedAt = now
	val, err := etcd.Marshal(n)
	if err != nil {
		return 0, err
	}
	cascade := nodeDeleteCascade(id, n.Name, string(val), certOps, reapOps, wgRec)
	if err := s.commitCascadeWithNodeIntent(ctx, cascade, intentKey, myRev); err != nil {
		// Final-chunk CAS lost (intent severed) or a txn error: the node row is NOT
		// soft-deleted. The head chunks and per-VM evacuations are idempotent/safe to
		// re-run; a retry re-derives a smaller union. (On a CAS loss the deferred
		// clear in DeleteNode is a guarded no-op.)
		return 0, fmt.Errorf("force-delete node cascade: %v", err)
	}
	return certsRevoked, nil
}

// nodeDeleteCascade assembles the ordered force-delete op slice. The
// migration-cancel and vm_runtime-orphan writes are NOT here - they commit
// earlier through their own per-row ModRevision CAS (cancelNodeMigrations /
// orphanNodeVMRuntimes) so a concurrent cutover wins; this cascade carries only
// idempotent puts/deletes. The whole cascade routes through commitInChunks: each
// <=120-op chunk commits atomically, so a crash leaves a clean PREFIX of
// cert-revoke/wg-purge/reap ops. The node-soft-delete ops (name-guard delete +
// nodePut) are appended LAST, the nodePut genuinely last, so the node row
// disappears only after every other op, and a retry re-derives the remaining
// work (NodeByID at the top returns ErrNotFound once the node row is gone). Every
// preceding op is an idempotent put/delete, so re-running the whole cascade on
// retry is safe.
//
// Ordering is load-bearing: the WireGuard fabric purge (record + pubkey guard)
// MUST precede the node-soft-delete. Were it to trail, a chunk boundary falling
// between the node-delete and the wg-purge plus a crash would soft-delete the
// node while leaking the agent_wireguard record + pubkey guard, which the retry
// can never re-run (the gone node short-circuits at NodeByID). There is no
// backstop reaper for agent_wireguard, so the leaked pubkey guard would later
// fail a node re-bootstrap with ErrAgentWireguardPubkeyInUse.
func nodeDeleteCascade(nodeID uuid.UUID, nodeName, nodeVal string, certOps, reapOps []clientv3.Op, wgRec *store.AgentWireguard) []clientv3.Op {
	cascade := make([]clientv3.Op, 0, len(certOps)+len(reapOps)+5)
	// Purge the node's WireGuard fabric record + pubkey guard so the dead node
	// stops appearing in the mesh and its pubkey becomes reusable - before the
	// node soft-delete so a retry can re-run it.
	if wgRec != nil {
		cascade = append(cascade,
			clientv3.OpDelete(agentWireguardKey(nodeID)),
			clientv3.OpDelete(agentWireguardPubkeyGuard(wgRec.PublicKey)),
		)
	}
	// Revoke the node's agent certs before the node soft-delete so a crash+retry
	// re-runs them (the gone node short-circuits at NodeByID once soft-deleted).
	cascade = append(cascade, certOps...)
	// Reap the node's gateway memberships (row + per-network index + tenant-IP
	// reservation) and its published-listener status rows before the node
	// soft-delete, for the same crash+retry reason: a chunk boundary falling
	// after the node-put plus a crash would leak those rows and reserved addresses
	// the gone node can never re-derive.
	cascade = append(cascade, reapOps...)
	// Prune the node's observed blob inventory so a deleted node stops counting as
	// an observed holder and leaks no phantom digests into the durability scan. A
	// single fixed delete op, ordered ahead of the node soft-delete for the same
	// crash+retry reason.
	cascade = append(cascade, clientv3.OpDelete(nodeBlobInventoryKey(nodeID)))
	// The name-guard delete precedes the nodePut so the nodePut (which flips the
	// row's DeletedAt and thus makes NodeByID short-circuit) is the genuine LAST
	// op: a crash before it leaves the node row present and a retry re-runs the
	// whole idempotent cascade.
	cascade = append(cascade,
		clientv3.OpDelete(nodeNameGuard(nodeName)),
		clientv3.OpPut(nodeKey(nodeID), nodeVal),
	)
	return cascade
}

// agentWireguardRecordForDelete loads the node's WireGuard fabric record so its
// purge ops can be ordered ahead of the node soft-delete (see nodeDeleteCascade).
// A nil record with a nil error means the node never joined the mesh (ErrNotFound)
// and has nothing to purge; any other error aborts the delete.
func (s *Store) agentWireguardRecordForDelete(ctx context.Context, id uuid.UUID) (*store.AgentWireguard, error) {
	rec, err := s.AgentWireguardByNodeID(ctx, id)
	switch {
	case err == nil:
		return &rec, nil
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	default:
		return nil, fmt.Errorf("load agent_wireguard for node delete: %v", err)
	}
}

// nodeDeleteReapOps returns the delete ops that purge the node's leaf state that
// has no per-node index and no backstop reaper: its gateway memberships and its
// published-listener status rows. They share the same ordering slot in
// nodeDeleteCascade (ahead of the node soft-delete) so a crash+retry re-runs them.
func (s *Store) nodeDeleteReapOps(ctx context.Context, id uuid.UUID) ([]clientv3.Op, error) {
	membershipOps, err := s.gatewayMembershipDeleteOps(ctx, id)
	if err != nil {
		return nil, err
	}
	listenerOps, err := s.collectLBPublishedListenerStatusOpsForNode(ctx, id)
	if err != nil {
		return nil, err
	}
	return append(membershipOps, listenerOps...), nil
}

// gatewayMembershipDeleteOps returns the delete ops that purge every gateway
// membership held by the node - the membership row, its per-network index, and
// its tenant-IP reservation - so a deleted gateway leaks neither membership rows
// nor reserved addresses. The ops are assembled here but ordered ahead of the
// node soft-delete by nodeDeleteCascade.
func (s *Store) gatewayMembershipDeleteOps(ctx context.Context, gatewayID uuid.UUID) ([]clientv3.Op, error) {
	memberships, err := s.ListGatewayMembershipsForGateway(ctx, gatewayID)
	if err != nil {
		return nil, fmt.Errorf("list gateway memberships for node delete: %v", err)
	}
	ops := make([]clientv3.Op, 0, len(memberships)*3)
	for _, m := range memberships {
		ops = append(ops,
			clientv3.OpDelete(gatewayMembershipKey(m.GatewayID, m.NetworkID)),
			clientv3.OpDelete(gatewayMembershipNetworkIndexKey(m.NetworkID, m.GatewayID)),
			clientv3.OpDelete(vmNicIPv4ReservationKey(m.NetworkID, m.TenantIP)),
		)
	}
	return ops, nil
}

// nodeEffective projects a node onto the effective-availability view, computing
// effective CPU/memory as raw availability minus pending reservations.
func (s *Store) nodeEffective(ctx context.Context, n store.Node) (store.NodeEffectiveAvailability, error) {
	e := store.NodeEffectiveAvailability{
		ID:                        n.ID,
		Name:                      n.Name,
		Architecture:              n.Architecture,
		AdvertisedEndpoint:        n.AdvertisedEndpoint,
		IngressAdvertisedEndpoint: n.IngressAdvertisedEndpoint,
		MigrationHost:             n.MigrationHost,
		MigrationPortRangeStart:   n.MigrationPortRangeStart,
		MigrationPortRangeEnd:     n.MigrationPortRangeEnd,
		Status:                    n.Status,
		GatewayRole:               n.GatewayRole,
		CordonedAt:                n.CordonedAt,
		CPUCoresTotal:             n.CPUCoresTotal,
		CPUCoresAvailable:         n.CPUCoresAvailable,
		CPUModel:                  n.CPUModel,
		CpuFlags:                  n.CpuFlags,
		MemoryTotalMib:            n.MemoryTotalMib,
		MemoryAvailableMib:        n.MemoryAvailableMib,
		Hugepages2mibTotal:        n.Hugepages2mibTotal,
		Hugepages1gibTotal:        n.Hugepages1gibTotal,
		KernelVersion:             n.KernelVersion,
		QEMUVersion:               n.QEMUVersion,
		NumaTopology:              n.NumaTopology,
		Capabilities:              n.Capabilities,
		LastHeartbeatAt:           n.LastHeartbeatAt,
		AgentVersion:              n.AgentVersion,
		Labels:                    n.Labels,
		MemoryPressureSince:       n.MemoryPressureSince,
		MemoryPressureCount:       n.MemoryPressureCount,
		SystemDiskTotalBytes:      n.SystemDiskTotalBytes,
		SystemDiskAvailableBytes:  n.SystemDiskAvailableBytes,
		SystemDiskPressureSince:   n.SystemDiskPressureSince,
		SystemDiskPressureCount:   n.SystemDiskPressureCount,
		CreatedAt:                 n.CreatedAt,
		UpdatedAt:                 n.UpdatedAt,
		DeletedAt:                 n.DeletedAt,
	}
	pendCPU, pendMem, err := s.pendingReservations(ctx, n)
	if err != nil {
		return store.NodeEffectiveAvailability{}, err
	}
	// Reserve capacity for VMs being live-migrated INTO this node, for the
	// migration's duration (spec crash-semantics). During an active migration
	// the VM stays pinned to the SOURCE until cutover (CommitMigrationCutover
	// flips PinnedNodeID to the target), so on the target node it is NOT in the
	// pinned_node index pendingReservations ranges - this reservation is the only
	// subtraction here and does not double-count. When the migration goes
	// terminal its node-index entry is deleted and the reservation disappears;
	// after cutover the VM is pinned to the target and counted by the pending
	// path instead.
	resvCPU, resvMem, err := s.migrationTargetReservations(ctx, n.ID)
	if err != nil {
		return store.NodeEffectiveAvailability{}, err
	}
	if n.CPUCoresAvailable != nil {
		v := max(*n.CPUCoresAvailable-pendCPU-resvCPU, 0)
		e.CPUCoresEffective = &v
	}
	if n.MemoryAvailableMib != nil {
		v := max(*n.MemoryAvailableMib-pendMem-resvMem, 0)
		e.MemoryEffectiveMib = &v
	}
	return e, nil
}

// migrationTargetReservations sums the cpu/memory of VMs being live-migrated
// into the node by an active (non-terminal) migration. It reuses the per-node
// migration index activeMigrationsOnNode reads (which already excludes terminal
// migrations) and keeps only migrations whose target is this node.
func (s *Store) migrationTargetReservations(ctx context.Context, nodeID uuid.UUID) (cpu int32, mem int64, err error) {
	migs, err := s.activeMigrationsOnNode(ctx, nodeID)
	if err != nil {
		return 0, 0, err
	}
	for _, m := range migs {
		if m.TargetNodeID == nil || *m.TargetNodeID != nodeID {
			continue
		}
		vm, gerr := s.VMByID(ctx, m.VmID)
		if gerr != nil {
			if errors.Is(gerr, store.ErrNotFound) {
				continue
			}
			return 0, 0, gerr
		}
		cpu += vm.CpuCores
		mem += int64(vm.MemoryMib)
	}
	return cpu, mem, nil
}

// pendingReservations sums the cpu/memory of vms pinned to the node that the
// agent has not yet observed (created after the node's last heartbeat),
// matching the lateral subquery of node_effective_availability.
func (s *Store) pendingReservations(ctx context.Context, n store.Node) (cpu int32, mem int64, err error) {
	items, err := s.c.Range(ctx, vmsPinnedNodeIndexPrefix(n.ID))
	if err != nil {
		return 0, 0, err
	}
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return 0, 0, fmt.Errorf("corrupt pinned-node index %q: %v", kv.Key, perr)
		}
		var vm store.VM
		found, gerr := s.c.GetJSON(ctx, vmKey(id), &vm)
		if gerr != nil {
			return 0, 0, gerr
		}
		if !found || vm.DeletedAt != nil || vm.DesiredPhase == store.VmDesiredPhaseDeleted {
			continue
		}
		if n.LastHeartbeatAt != nil && !vm.CreatedAt.After(*n.LastHeartbeatAt) {
			continue
		}
		cpu += vm.CpuCores
		mem += int64(vm.MemoryMib)
	}
	return cpu, mem, nil
}

// ListVMRefsForNodeDeclared returns the VMs pinned (desired home) to the node,
// with ids, for the drain saga. It ranges vmsPinnedNodeIndexPrefix (NOT the
// runtime-by-node index), so a pinned-but-not-yet-observed VM is included.
// Soft-deleted and desired-deleted VMs are excluded.
func (s *Store) ListVMRefsForNodeDeclared(ctx context.Context, nodeID uuid.UUID) ([]store.NodeVMRef, error) {
	items, err := s.c.Range(ctx, vmsPinnedNodeIndexPrefix(nodeID))
	if err != nil {
		return nil, err
	}
	var refs []store.NodeVMRef
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return nil, fmt.Errorf("corrupt pinned-node index %q: %v", kv.Key, perr)
		}
		var vm store.VM
		found, gerr := s.c.GetJSON(ctx, vmKey(id), &vm)
		if gerr != nil {
			return nil, gerr
		}
		if !found || vm.DeletedAt != nil || vm.DesiredPhase == store.VmDesiredPhaseDeleted {
			continue
		}
		refs = append(refs, store.NodeVMRef{ID: vm.ID, Name: vm.Name, DesiredPhase: vm.DesiredPhase})
	}
	return refs, nil
}

// activeMigrationsOnNode returns the non-terminal migrations touching the node,
// resolved from the node migration index by reading each primary.
func (s *Store) activeMigrationsOnNode(ctx context.Context, nodeID uuid.UUID) ([]store.Migration, error) {
	items, err := s.c.Range(ctx, migrationsNodeIndexPrefix(nodeID))
	if err != nil {
		return nil, err
	}
	var active []store.Migration
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return nil, fmt.Errorf("corrupt migration node index %q: %v", kv.Key, perr)
		}
		var m store.Migration
		found, gerr := s.c.GetJSON(ctx, migrationKey(id), &m)
		if gerr != nil {
			return nil, gerr
		}
		if !found || isTerminalMigration(m.Phase) {
			continue
		}
		active = append(active, m)
	}
	return active, nil
}

// ActiveSourceMigrationCount counts active (non-terminal) migrations whose
// SOURCE is nodeID - the in-flight evacuations the drain saga has already
// started, used to bound per-drain concurrency.
func (s *Store) ActiveSourceMigrationCount(ctx context.Context, nodeID uuid.UUID) (int, error) {
	migs, err := s.activeMigrationsOnNode(ctx, nodeID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range migs {
		if m.SourceNodeID != nil && *m.SourceNodeID == nodeID {
			n++
		}
	}
	return n, nil
}

// nodeDeleteCASAttempts bounds the per-row CAS retries the force-delete cancel /
// orphan writers make against a concurrently-churning migration or runtime row.
// On exhaustion the writer returns an error and DeleteNode aborts (never
// soft-deletes the node) so a genuinely-active migration cannot be stranded with
// a dangling active-per-VM guard.
const nodeDeleteCASAttempts = 5

// cancelNodeMigrations cancels each still-active migration touching the node
// being force-deleted, through the shared CancelMigration CAS (which stamps the
// reason + completed_at and releases the active-per-VM guard + per-node indexes
// via terminalCleanupOps). It re-reads each row under CancelMigration rather than
// trusting the snapshot in migs, so a migration a concurrent cutover already
// drove terminal is SKIPPED (not clobbered back to cancelled - fail toward
// inaction). Returns the count actually cancelled. A migration that keeps losing
// the CAS to a live progress stream past nodeDeleteCASAttempts returns an error,
// aborting the delete before the node soft-delete (the migration is genuinely
// active; do not delete its node out from under it).
func (s *Store) cancelNodeMigrations(ctx context.Context, migs []store.Migration, reason string) (int64, error) {
	var n int64
	for _, mig := range migs {
		for attempt := 0; ; attempt++ {
			_, err := s.CancelMigration(ctx, mig.ID, reason)
			switch {
			case err == nil:
				n++
			case errors.Is(err, store.ErrMigrationNotCancelable):
				// Already terminal: a cutover/failure/cancel won. Do not flip it.
			case errors.Is(err, store.ErrNotFound):
				// Row vanished (retention/delete): nothing to cancel.
			case errors.Is(err, store.ErrConcurrentUpdate):
				if attempt+1 >= nodeDeleteCASAttempts {
					return n, fmt.Errorf("cancel migration %s for node delete: %w", mig.ID, err)
				}
				continue // re-read + re-CAS
			default:
				return n, fmt.Errorf("cancel migration %s for node delete: %v", mig.ID, err)
			}
			break
		}
	}
	return n, nil
}

// orphanOneVMRuntime orphans a single vm_runtime under a ModRevision CAS, or
// skips it (reaping only the stale index entry) when the row is gone or has moved
// off nodeID. Returns whether it actually orphaned the row.
func (s *Store) orphanOneVMRuntime(ctx context.Context, vmID, nodeID uuid.UUID, indexKey string) (bool, error) {
	for attempt := 0; ; attempt++ {
		resp, err := s.c.Raw().Get(ctx, vmRuntimeKey(vmID))
		if err != nil {
			return false, err
		}
		if len(resp.Kvs) == 0 {
			// Runtime row gone (a delete projection won): drop the stale by-node
			// index entry (idempotent) and skip.
			_, derr := s.c.Delete(ctx, indexKey)
			return false, derr
		}
		rev := resp.Kvs[0].ModRevision
		var rt store.VMRuntime
		if err := json.Unmarshal(resp.Kvs[0].Value, &rt); err != nil {
			return false, fmt.Errorf("unmarshal vm_runtime %s: %v", vmID, err)
		}
		if rt.CurrentNodeID == nil || *rt.CurrentNodeID != nodeID {
			// Moved off this node (a cutover landed) or already orphaned: do not
			// clobber. The stale by-node index entry under this node is reaped
			// (a cutover already deletes it; this is a defensive idempotent no-op).
			_, derr := s.c.Delete(ctx, indexKey)
			return false, derr
		}
		rt.Phase = store.VmPhaseOrphaned
		rt.CurrentNodeID = nil
		val, err := etcd.Marshal(rt)
		if err != nil {
			return false, err
		}
		txResp, err := s.c.Raw().Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(vmRuntimeKey(vmID)), "=", rev)).
			Then(
				clientv3.OpPut(vmRuntimeKey(vmID), string(val)),
				clientv3.OpDelete(indexKey),
			).
			Commit()
		if err != nil {
			return false, fmt.Errorf("orphan vm_runtime %s txn: %v", vmID, err)
		}
		if txResp.Succeeded {
			return true, nil
		}
		if attempt+1 >= nodeDeleteCASAttempts {
			return false, fmt.Errorf("orphan vm_runtime %s for node delete: %w", vmID, store.ErrConcurrentUpdate)
		}
	}
}

// isTerminalMigration reports whether a migration phase is terminal (matches the
// SQL "phase not in ('completed','failed','cancelled')" active predicate).
func isTerminalMigration(p store.MigrationPhase) bool {
	switch p {
	case store.MigrationPhaseCompleted, store.MigrationPhaseFailed, store.MigrationPhaseCancelled:
		return true
	default:
		return false
	}
}
