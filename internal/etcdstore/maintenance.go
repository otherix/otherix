// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
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
		if !s.decodeOrQuarantine(ctx, kv.Key, kv.Value, &t, "task") {
			continue
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

// DeleteExpiredMigrations removes TERMINAL migrations past their per-state
// retention window (completed past CompletedCutoff; failed past FailedCutoff;
// cancelled past CancelledCutoff), dropping each migration's per-VM history index
// entry alongside the row. Only terminal migrations are ever deleted - a
// non-terminal migration (pending/setup/active/postcopy_active) is IN FLIGHT (the
// VM is still on its source) and is never swept regardless of age. Mirrors
// DeleteExpiredTasks. Returns the number of migrations deleted.
//
// A terminal migration has already released its active-VM guard and per-node
// indexes at the terminal transition (terminalCleanupOps); only the row and the
// per-VM history index remain. terminalCleanupOps(m) is appended defensively so an
// older row that never released those keys is fully cleaned up - every op is a
// delete, idempotent when the key is already gone.
func (s *Store) DeleteExpiredMigrations(ctx context.Context, arg store.DeleteExpiredMigrationsParams) (int64, error) {
	items, err := s.c.Range(ctx, etcd.Key("migrations")+"/")
	if err != nil {
		return 0, err
	}
	var (
		ops     []clientv3.Op
		deleted int64
	)
	for _, kv := range items {
		var m store.Migration
		if !s.decodeOrQuarantine(ctx, kv.Key, kv.Value, &m, "migration") {
			continue
		}
		if !migrationRetentionExpired(m, arg) {
			continue
		}
		ops = append(ops, clientv3.OpDelete(migrationKey(m.ID)))
		ops = append(ops, clientv3.OpDelete(migrationVMIndexKey(m.VmID, m.ID)))
		ops = append(ops, terminalCleanupOps(m)...)
		deleted++
	}
	if err := s.commitInChunks(ctx, ops); err != nil {
		return 0, fmt.Errorf("delete expired migrations: %v", err)
	}
	return deleted, nil
}

// migrationRetentionExpired reports whether a TERMINAL migration is past its
// per-state retention cutoff. Non-terminal phases (pending/setup/active/
// postcopy_active) are never expired - they are in flight and must survive any
// sweep. The terminal timestamp is CompletedAt when set, else UpdatedAt (a
// terminal transition always stamps both, but UpdatedAt is the safe fallback for
// any legacy row). Mirrors taskRetentionExpired.
func migrationRetentionExpired(m store.Migration, arg store.DeleteExpiredMigrationsParams) bool {
	terminalAt := m.UpdatedAt
	if m.CompletedAt != nil {
		terminalAt = *m.CompletedAt
	}
	switch m.Phase {
	case store.MigrationPhaseCompleted:
		return terminalAt.Before(arg.CompletedCutoff)
	case store.MigrationPhaseFailed:
		return terminalAt.Before(arg.FailedCutoff)
	case store.MigrationPhaseCancelled:
		return terminalAt.Before(arg.CancelledCutoff)
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
		if !s.decodeOrQuarantine(ctx, kv.Key, kv.Value, &j, "job") {
			continue
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

// Job lease constants govern crash-orphan reclaim. A worker renews its running
// job's lease every JobLeaseRenewInterval; the reaper reclaims a running job
// whose lease is older than JobLease. JobLease is 3x the renew interval, so a
// healthy long-running job (a live migration renews every 30s) never crosses the
// lease, while a crashed worker's job is reclaimed within ~JobLease of the crash.
const (
	JobLeaseRenewInterval = 30 * time.Second
	JobLease              = 90 * time.Second
)

// ReclaimStaleRunningJobs returns crash-orphaned running jobs to pending. A
// running job is presumed orphaned when its ClaimedAt lease is missing (a
// legacy/pre-upgrade row with no live renewer) or older than olderThan (its
// worker stopped renewing). Each reclaim re-reads the job's mod-revision and
// commits its OWN compare-and-set transaction (ModRevision compare). On the
// authoritative re-read it re-validates BOTH the running state AND the lease age
// before the CAS, so a job whose lease was renewed (or that completed) between
// the snapshot scan and the per-job read is left untouched: the per-job CAS only
// guards against writes landing after the read, so without the re-read predicate
// a renewal landing between Range and Get would carry a fresh mod-revision that
// the CAS accepts, bouncing a healthy just-renewed job back to pending. Attempts
// is left UNCHANGED - a crash is not a real failure and must not consume the
// retry budget. Returns the number of jobs reclaimed.
func (s *Store) ReclaimStaleRunningJobs(ctx context.Context, olderThan time.Time) (int64, error) {
	items, err := s.c.Range(ctx, jobsPrefix())
	if err != nil {
		return 0, err
	}
	var reclaimed int64
	for _, kv := range items {
		var j Job
		if !s.decodeOrQuarantine(ctx, kv.Key, kv.Value, &j, "job") {
			continue
		}
		if j.State != JobStateRunning {
			continue
		}
		if j.ClaimedAt != nil && !j.ClaimedAt.Before(olderThan) {
			continue
		}
		job, modRev, found, rerr := s.jobWithRev(ctx, j.ID)
		if rerr != nil {
			return reclaimed, rerr
		}
		if !found || job.State != JobStateRunning {
			continue
		}
		// Re-validate the lease age on the authoritative re-read (fail-safe): a
		// renewal landing between the Range snapshot and this Get carries a fresh
		// ClaimedAt and a new mod-revision the CAS would otherwise accept, bouncing
		// a healthy just-renewed job back to pending. Respect the renewal instead.
		if job.ClaimedAt != nil && !job.ClaimedAt.Before(olderThan) {
			continue
		}
		job.State = JobStatePending
		job.ClaimedAt = nil
		val, merr := etcd.Marshal(job)
		if merr != nil {
			return reclaimed, merr
		}
		resp, cerr := s.c.Raw().Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(jobKey(j.ID)), "=", modRev)).
			Then(clientv3.OpPut(jobKey(j.ID), string(val))).
			Commit()
		if cerr != nil {
			return reclaimed, fmt.Errorf("reclaim job %d: %v", j.ID, cerr)
		}
		if resp.Succeeded {
			reclaimed++
		}
	}
	return reclaimed, nil
}

// ReclaimStaleJobsFunc returns a periodic function that reclaims crash-orphaned
// running jobs whose lease is older than lease. The scheduler logs any returned
// error, so this only logs the reclaimed count.
func ReclaimStaleJobsFunc(st *Store, lease time.Duration, log *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		n, err := st.ReclaimStaleRunningJobs(ctx, time.Now().UTC().Add(-lease))
		if err != nil {
			return fmt.Errorf("reclaim stale running jobs: %v", err)
		}
		if n > 0 {
			log.InfoContext(ctx, "jobs.reclaim", "reclaimed", n)
		}
		return nil
	}
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
		if !s.decodeOrQuarantine(ctx, kv.Key, kv.Value, &st, "network_node_status") {
			continue
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
//
// Each promotion re-reads the node and commits through casNodeStatus (a
// ModRevision CAS), so it preserves every other field - notably a gateway role
// bit toggled between the selection snapshot and this write - and refuses any
// node that now carries an active drain (DrainTaskID set). The source status is
// filtered from the snapshot; a node that lost the CAS or gained a drain is
// skipped, and the next sweep retries it, so one node's skip never aborts the
// pass.
func (s *Store) PromoteHealthyNodes(ctx context.Context, freshAfter time.Time) ([]store.PromoteHealthyNodesRow, error) {
	nodes, err := s.liveNodes(ctx)
	if err != nil {
		return nil, err
	}
	var rows []store.PromoteHealthyNodesRow
	for _, n := range nodes {
		if n.Status != store.NodeStatusPending && n.Status != store.NodeStatusUnreachable {
			continue
		}
		if n.LastHeartbeatAt == nil || n.LastHeartbeatAt.Before(freshAfter) {
			continue
		}
		committed, written, err := s.casNodeStatus(ctx, n.ID, store.NodeStatusReady)
		if err != nil {
			return nil, err
		}
		if !written {
			continue
		}
		// GatewayRole is sourced from the committed node (casNodeStatus re-reads
		// fresh), not the pre-CAS snapshot, so the default-pool ready-hook keys
		// its gateway skip on the just-written role bit.
		rows = append(rows, store.PromoteHealthyNodesRow{ID: n.ID, Name: n.Name, Status: store.NodeStatusReady, GatewayRole: committed.GatewayRole})
	}
	return rows, nil
}

// MarkNodesUnreachable demotes nodes in 'ready' or 'pending' whose heartbeat is
// missing or older than staleBefore to 'unreachable', returning the affected
// rows. 'cordoned' and 'draining' are operator-pinned and untouched.
//
// Each demotion re-reads the node and commits under a ModRevision CAS, refusing
// any node that now carries an active drain (DrainTaskID set). The selection
// snapshot is taken from liveNodes, but a drain can flip a ready node to
// draining between that snapshot and this write; a blind put would then drop the
// drain_task_id and strand the saga. A node that lost the CAS or gained a drain
// is skipped (the next sweep retries it), so one node's skip never aborts the
// rest of the pass.
func (s *Store) MarkNodesUnreachable(ctx context.Context, staleBefore time.Time) ([]store.MarkNodesUnreachableRow, error) {
	nodes, err := s.liveNodes(ctx)
	if err != nil {
		return nil, err
	}
	var rows []store.MarkNodesUnreachableRow
	for _, n := range nodes {
		if n.Status != store.NodeStatusReady && n.Status != store.NodeStatusPending {
			continue
		}
		if n.LastHeartbeatAt != nil && !n.LastHeartbeatAt.Before(staleBefore) {
			continue
		}
		_, written, err := s.casNodeStatus(ctx, n.ID, store.NodeStatusUnreachable)
		if err != nil {
			return nil, err
		}
		if !written {
			continue
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
	for _, n := range nodes {
		if n.Status != store.NodeStatusUnreachable {
			continue
		}
		// A node that has never sent a heartbeat is measured from when it joined
		// (created_at), not treated as infinitely stale. Otherwise a freshly
		// joined node still finishing its bootstrap would be advanced straight to
		// the terminal 'gone' before it ever reports in - a slow first heartbeat
		// must get the full grace window, and this transition fails toward
		// inaction (the node lingers in the reversible 'unreachable' instead).
		staleSince := n.LastHeartbeatAt
		if staleSince == nil {
			staleSince = &n.CreatedAt
		}
		if !staleSince.Before(goneBefore) {
			continue
		}
		_, written, err := s.casNodeStatus(ctx, n.ID, store.NodeStatusGone)
		if err != nil {
			return nil, err
		}
		if !written {
			continue
		}
		rows = append(rows, store.MarkNodesGoneRow{ID: n.ID, Name: n.Name})
	}
	return rows, nil
}

// casNodeStatus flips a single node to status under a ModRevision CAS, returning
// the committed node and whether the write committed. It re-reads the row fresh
// (so the write rides the row's current revision) and refuses - reporting a zero
// node and false, no write - when the node now carries an active drain
// (DrainTaskID set): a heartbeat-reconcile demotion must never overwrite a live
// drain, which would drop the drain_task_id and strand the saga. A lost CAS
// (concurrent mutation) or a vanished node also reports false. The caller skips a
// false result and retries on the next sweep, so one node's refusal never aborts
// the batch. The node's prior status is not re-validated here; the caller already
// filtered the source status from its snapshot, and the CAS rejects any write
// that races a status change. The returned node carries the fresh, just-committed
// field values (e.g. the gateway role bit), so a caller keying a decision on them
// reads the committed state, not its pre-CAS snapshot.
func (s *Store) casNodeStatus(ctx context.Context, id uuid.UUID, status store.NodeStatus) (store.Node, bool, error) {
	n, modRev, err := s.nodeWithRev(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Node{}, false, nil
		}
		return store.Node{}, false, err
	}
	if n.DrainTaskID != nil {
		return store.Node{}, false, nil
	}
	n.Status = status
	n.UpdatedAt = time.Now().UTC()
	val, err := etcd.Marshal(n)
	if err != nil {
		return store.Node{}, false, err
	}
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(nodeKey(id)), "=", modRev)).
		Then(clientv3.OpPut(nodeKey(id), string(val))).
		Commit()
	if err != nil {
		return store.Node{}, false, fmt.Errorf("set node status txn: %v", err)
	}
	if !resp.Succeeded {
		return store.Node{}, false, nil
	}
	return n, true, nil
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
		if !s.decodeOrQuarantine(ctx, kv.Key, kv.Value, &n, "node") {
			continue
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
		if !s.decodeOrQuarantine(ctx, kv.Key, kv.Value, &p, "storage_pool") {
			continue
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
		if !s.decodeOrQuarantine(ctx, kv.Key, kv.Value, &t, "task") {
			continue
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
