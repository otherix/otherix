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
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// Migrations are written through one atomic transaction: the primary row, the
// per-VM active guard, the per-node and per-VM secondary indexes, and the
// backing task+job all commit together (CreateMigration). The per-node index
// leaf matches migrationsNodeIndexPrefix (nodes.go) so activeMigrationsOnNode
// resolves the rows this path writes; the read/cancel cascade lives in nodes.go.

// migrationActiveVMGuard is the at-most-one-active-migration-per-VM uniqueness
// key. A non-terminal migration holds it; CreateMigration's CAS on its
// CreateRevision==0 fails when a migration for the VM is already active.
func migrationActiveVMGuard(vmID uuid.UUID) string {
	return etcd.Key("uniq", "migration_active_vm", vmID.String())
}

// migrationNodeIndexKey is the per-node index leaf for a migration touching the
// node as source or target. Its prefix matches migrationsNodeIndexPrefix
// (nodes.go), and its value is the migration id, so activeMigrationsOnNode
// ranges the prefix and parses the value to resolve each primary.
func migrationNodeIndexKey(node, id uuid.UUID) string {
	return etcd.Key("index", "migrations", "node", node.String(), id.String())
}

// migrationVMIndexKey is the per-VM index leaf for a migration, valued with the
// migration id (the per-VM migration list reads it).
func migrationVMIndexKey(vm, id uuid.UUID) string {
	return etcd.Key("index", "migrations", "vm", vm.String(), id.String())
}

// migrationsVMIndexPrefix lists every migration for a VM (maintained by
// CreateMigration) - consumed by ListMigrations' VM filter, which reads each
// primary. Mirrors migrationsNodeIndexPrefix (nodes.go).
func migrationsVMIndexPrefix(vm uuid.UUID) string {
	return etcd.Key("index", "migrations", "vm", vm.String()) + "/"
}

// CreateMigration writes a pending migration row, its per-VM active guard, the
// per-node (source/target) and per-VM secondary indexes, and the backing
// task+job in one transaction. The CAS on the guard's CreateRevision==0 fails
// closed when the VM already has an active migration, returning
// store.ErrMigrationActiveExists. Source/target node index entries are written
// only for the node ids that are set.
func (s *Store) CreateMigration(ctx context.Context, p store.CreateMigrationParams, args queue.JobArgs) (store.Migration, error) {
	now := time.Now().UTC()
	m := store.Migration{
		ID:                p.ID,
		VmID:              p.VmID,
		SourceNodeID:      p.SourceNodeID,
		TargetNodeID:      p.TargetNodeID,
		InitiatedByUserID: p.InitiatedByUserID,
		Reason:            p.Reason,
		Phase:             store.MigrationPhasePending,
		MaxBandwidthBytes: p.MaxBandwidthBytes,
		MaxDowntimeMs:     p.MaxDowntimeMs,
		Live:              p.Live,
		AllowPostcopy:     p.AllowPostcopy,
		TargetPoolName:    p.TargetPoolName,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	val, err := etcd.Marshal(m)
	if err != nil {
		return store.Migration{}, err
	}
	seq, jobOp, err := s.enqueueJobOp(ctx, args)
	if err != nil {
		return store.Migration{}, err
	}
	task := taskFromParams(p.Task, seq)
	taskVal, err := etcd.Marshal(task)
	if err != nil {
		return store.Migration{}, err
	}

	guard := migrationActiveVMGuard(p.VmID)
	ops := []clientv3.Op{
		clientv3.OpPut(guard, p.ID.String()),
		clientv3.OpPut(migrationKey(m.ID), string(val)),
		clientv3.OpPut(migrationVMIndexKey(p.VmID, m.ID), m.ID.String()),
		clientv3.OpPut(taskKey(task.ID), string(taskVal)),
		jobOp,
	}
	if p.SourceNodeID != nil {
		ops = append(ops, clientv3.OpPut(migrationNodeIndexKey(*p.SourceNodeID, m.ID), m.ID.String()))
	}
	if p.TargetNodeID != nil {
		ops = append(ops, clientv3.OpPut(migrationNodeIndexKey(*p.TargetNodeID, m.ID), m.ID.String()))
	}
	ops = append(ops, taskIndexOps(task)...)

	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(ops...).
		Commit()
	if err != nil {
		return store.Migration{}, fmt.Errorf("create migration txn: %v", err)
	}
	if !resp.Succeeded {
		return store.Migration{}, store.ErrMigrationActiveExists
	}
	return m, nil
}

// terminalCleanupOps releases a migration's transient state when it reaches a
// terminal phase: the active-per-VM guard and the per-node index entries. The
// per-VM index is intentionally KEPT so terminal migrations remain listable by
// VM for history. The primary row is kept (the caller writes its terminal form).
func terminalCleanupOps(m store.Migration) []clientv3.Op {
	ops := []clientv3.Op{clientv3.OpDelete(migrationActiveVMGuard(m.VmID))}
	if m.SourceNodeID != nil {
		ops = append(ops, clientv3.OpDelete(migrationNodeIndexKey(*m.SourceNodeID, m.ID)))
	}
	if m.TargetNodeID != nil {
		ops = append(ops, clientv3.OpDelete(migrationNodeIndexKey(*m.TargetNodeID, m.ID)))
	}
	return ops
}

// CommitMigrationCutover atomically re-pins the VM from source to target and
// marks the migration completed. It is the safety-critical seam of live
// migration (spec D3): PinnedNodeID stays = source until this single Txn flips
// it. The Txn CAS-guards BOTH the migration and VM ModRevisions, so any
// concurrent write to either row loses the commit and returns
// store.ErrConcurrentUpdate (the worker re-reads and retries). The pinned_node
// index move byte-matches buildBindTxn's leaf form. Terminal locks are released
// via terminalCleanupOps. A migration already completed is a no-op (idempotent
// reconcile); any other terminal phase, or a nil target, is an error - a
// terminal-failed/cancelled migration must never re-pin (fail-safe-to-source).
func (s *Store) CommitMigrationCutover(ctx context.Context, migID uuid.UUID) error {
	mResp, err := s.c.Raw().Get(ctx, migrationKey(migID))
	if err != nil {
		return err
	}
	if len(mResp.Kvs) == 0 {
		return store.ErrNotFound
	}
	mRev := mResp.Kvs[0].ModRevision
	var m store.Migration
	if err := json.Unmarshal(mResp.Kvs[0].Value, &m); err != nil {
		return err
	}
	if m.Phase == store.MigrationPhaseCompleted {
		return nil // idempotent reconcile: cutover already committed.
	}
	if isTerminalMigration(m.Phase) {
		return store.ErrMigrationTerminal
	}
	if m.TargetNodeID == nil {
		return fmt.Errorf("cutover without target for migration %s", migID)
	}

	vResp, err := s.c.Raw().Get(ctx, vmKey(m.VmID))
	if err != nil {
		return err
	}
	if len(vResp.Kvs) == 0 {
		return store.ErrNotFound
	}
	vRev := vResp.Kvs[0].ModRevision
	var vm store.VM
	if err := json.Unmarshal(vResp.Kvs[0].Value, &vm); err != nil {
		return err
	}

	// Read the runtime row so the cutover can move current_node_id in the same
	// Txn. Guard its ModRevision so a racing heartbeat upsert loses.
	rtResp, err := s.c.Raw().Get(ctx, vmRuntimeKey(m.VmID))
	if err != nil {
		return err
	}
	var rt store.VMRuntime
	rtFound := len(rtResp.Kvs) > 0
	var rtRev int64
	if rtFound {
		rtRev = rtResp.Kvs[0].ModRevision
		if err := json.Unmarshal(rtResp.Kvs[0].Value, &rt); err != nil {
			return err
		}
	}

	now := time.Now().UTC()
	old := vm.PinnedNodeID
	target := *m.TargetNodeID
	vm.PinnedNodeID = &target
	vm.UpdatedAt = now

	m.Phase = store.MigrationPhaseCompleted
	m.CompletedAt = &now
	m.ProgressPercent = 100
	m.SchedulingReason = nil
	m.UpdatedAt = now

	vmVal, err := etcd.Marshal(vm)
	if err != nil {
		return err
	}
	mVal, err := etcd.Marshal(m)
	if err != nil {
		return err
	}

	ops := []clientv3.Op{
		clientv3.OpPut(vmKey(vm.ID), string(vmVal)),
		clientv3.OpPut(migrationKey(m.ID), string(mVal)),
	}
	ops = append(ops, pinnedNodeIndexOps(old, target, vm.ID)...)
	rtOps, err := cutoverRuntimeOps(rt, rtFound, old, target)
	if err != nil {
		return err
	}
	ops = append(ops, rtOps...)
	ops = append(ops, terminalCleanupOps(m)...)

	conds := []clientv3.Cmp{
		clientv3.Compare(clientv3.ModRevision(migrationKey(m.ID)), "=", mRev),
		clientv3.Compare(clientv3.ModRevision(vmKey(vm.ID)), "=", vRev),
	}
	if rtFound {
		conds = append(conds, clientv3.Compare(clientv3.ModRevision(vmRuntimeKey(m.VmID)), "=", rtRev))
	} else {
		conds = append(conds, clientv3.Compare(clientv3.CreateRevision(vmRuntimeKey(m.VmID)), "=", 0))
	}

	resp, err := s.c.Raw().Txn(ctx).If(conds...).Then(ops...).Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return store.ErrConcurrentUpdate
	}
	return nil
}

// pinnedNodeIndexOps returns the secondary-index mutations that move a VM's
// pinned_node entry from old (when set) to target. It OMITS the delete when
// old == target so the enclosing Txn never carries two operations on the same
// index key: a concurrent cutover can produce a torn read (the migration Get
// sees the migration still non-terminal while the VM Get already sees it pinned
// to target by a prior winner), and an unconditional delete+put on one key
// makes etcd reject the whole Txn with "duplicate key given in txn request" - a
// raw error leaking out before the CAS conditions are evaluated, instead of the
// ErrConcurrentUpdate the stale-ModRevision compare would return. The put alone
// is idempotent, so skipping the redundant delete keeps the index correct and
// lets the CAS decide the outcome.
func pinnedNodeIndexOps(old *uuid.UUID, target, vmID uuid.UUID) []clientv3.Op {
	var ops []clientv3.Op
	if old != nil && *old != target {
		ops = append(ops, clientv3.OpDelete(etcd.Key("index", "vms", "pinned_node", old.String(), vmID.String())))
	}
	return append(ops, clientv3.OpPut(etcd.Key("index", "vms", "pinned_node", target.String(), vmID.String()), vmID.String()))
}

// cutoverRuntimeOps moves vm_runtime.current_node_id source->target and the
// vm_runtime-by-node secondary index, preserving every other runtime field.
// It is part of the cutover Txn so the FDB-keying field flips atomically with
// PinnedNodeID (ADR 0035 req 3). A missing runtime row (VM never reported) means
// nothing to move - returns no ops.
func cutoverRuntimeOps(rt store.VMRuntime, found bool, source *uuid.UUID, target uuid.UUID) ([]clientv3.Op, error) {
	if !found {
		return nil, nil
	}
	rt.CurrentNodeID = &target
	val, err := etcd.Marshal(rt)
	if err != nil {
		return nil, err
	}
	ops := []clientv3.Op{clientv3.OpPut(vmRuntimeKey(rt.VmID), string(val))}
	if source != nil && *source != target {
		ops = append(ops, clientv3.OpDelete(vmRuntimeNodeIndexKey(*source, rt.VmID)))
	}
	ops = append(ops, clientv3.OpPut(vmRuntimeNodeIndexKey(target, rt.VmID), rt.VmID.String()))
	return ops, nil
}

// applyMigrationProgress copies the non-nil fields of upd onto m. SchedulingReason
// is tri-state: ClearSchedulingReason wins (sets nil), else a non-nil pointer
// sets it, else it is left untouched. UpdatedAt/CompletedAt/Phase-terminal
// handling stay with the caller.
func applyMigrationProgress(m *store.Migration, upd store.MigrationProgressUpdate) {
	if upd.Phase != nil {
		m.Phase = *upd.Phase
	}
	if upd.ProgressPercent != nil {
		m.ProgressPercent = *upd.ProgressPercent
	}
	if upd.BytesTotal != nil {
		m.BytesTotal = upd.BytesTotal
	}
	if upd.BytesTransferred != nil {
		m.BytesTransferred = *upd.BytesTransferred
	}
	if upd.BytesRemaining != nil {
		m.BytesRemaining = upd.BytesRemaining
	}
	if upd.ErrorMessage != nil {
		m.ErrorMessage = upd.ErrorMessage
	}
	if upd.StartedAt != nil {
		m.StartedAt = upd.StartedAt
	}
	if upd.LastScheduleAttemptAt != nil {
		m.LastScheduleAttemptAt = upd.LastScheduleAttemptAt
	}
	if upd.ClearSchedulingReason {
		m.SchedulingReason = nil
	} else if upd.SchedulingReason != nil {
		m.SchedulingReason = upd.SchedulingReason
	}
}

// UpdateMigrationProgress applies the non-nil fields of upd to a non-terminal
// migration under a single ModRevision CAS. A terminal migration is rejected
// with store.ErrMigrationTerminal (the cutover path owns the completed
// transition; this path owns pending/setup/active progress and the
// failed/cancelled terminal transitions). When the new phase is terminal
// (failed or cancelled), it stamps CompletedAt and appends terminalCleanupOps to
// release the active-per-VM guard and per-node indexes - PinnedNodeID is never
// touched here, so a failed/cancelled migration leaves the VM on its source
// (fail-safe-to-source, spec D3). A lost CAS returns store.ErrConcurrentUpdate.
func (s *Store) UpdateMigrationProgress(ctx context.Context, migID uuid.UUID, upd store.MigrationProgressUpdate) error {
	resp, err := s.c.Raw().Get(ctx, migrationKey(migID))
	if err != nil {
		return err
	}
	if len(resp.Kvs) == 0 {
		return store.ErrNotFound
	}
	rev := resp.Kvs[0].ModRevision
	var m store.Migration
	if err := json.Unmarshal(resp.Kvs[0].Value, &m); err != nil {
		return err
	}
	if isTerminalMigration(m.Phase) {
		return store.ErrMigrationTerminal
	}

	now := time.Now().UTC()
	applyMigrationProgress(&m, upd)
	m.UpdatedAt = now

	terminal := upd.Phase != nil && isTerminalMigration(*upd.Phase)
	if terminal {
		m.CompletedAt = &now
	}

	val, err := etcd.Marshal(m)
	if err != nil {
		return err
	}
	ops := []clientv3.Op{clientv3.OpPut(migrationKey(m.ID), string(val))}
	if terminal {
		ops = append(ops, terminalCleanupOps(m)...)
	}

	txResp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(migrationKey(m.ID)), "=", rev)).
		Then(ops...).
		Commit()
	if err != nil {
		return err
	}
	if !txResp.Succeeded {
		return store.ErrConcurrentUpdate
	}
	return nil
}

// CancelMigration marks a non-terminal migration cancelled with the audit
// reason under a single ModRevision CAS. Cancel is valid only pre-cutover (spec
// D5): a migration already in a terminal phase (completed/failed/cancelled) is
// rejected with store.ErrMigrationNotCancelable and the current row is returned
// unchanged. On success it stamps CompletedAt, clears SchedulingReason, and
// appends terminalCleanupOps to release the active-per-VM guard and per-node
// indexes. PinnedNodeID is never touched here - the VM was never moved off its
// source, so cancel is trivially fail-safe-to-source. A lost CAS returns
// store.ErrConcurrentUpdate.
func (s *Store) CancelMigration(ctx context.Context, id uuid.UUID, reason string) (store.Migration, error) {
	resp, err := s.c.Raw().Get(ctx, migrationKey(id))
	if err != nil {
		return store.Migration{}, err
	}
	if len(resp.Kvs) == 0 {
		return store.Migration{}, store.ErrNotFound
	}
	rev := resp.Kvs[0].ModRevision
	var m store.Migration
	if err := json.Unmarshal(resp.Kvs[0].Value, &m); err != nil {
		return store.Migration{}, err
	}
	if isTerminalMigration(m.Phase) {
		return m, store.ErrMigrationNotCancelable
	}

	now := time.Now().UTC()
	m.Phase = store.MigrationPhaseCancelled
	m.ErrorMessage = &reason
	m.CompletedAt = &now
	m.SchedulingReason = nil
	m.UpdatedAt = now

	val, err := etcd.Marshal(m)
	if err != nil {
		return store.Migration{}, err
	}
	ops := []clientv3.Op{clientv3.OpPut(migrationKey(m.ID), string(val))}
	ops = append(ops, terminalCleanupOps(m)...)

	txResp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(migrationKey(m.ID)), "=", rev)).
		Then(ops...).
		Commit()
	if err != nil {
		return store.Migration{}, err
	}
	if !txResp.Succeeded {
		return store.Migration{}, store.ErrConcurrentUpdate
	}
	return m, nil
}

// BindMigrationTarget binds the scheduler-picked target onto a node-less
// migration: it sets TargetNodeID (and TargetPoolName), clears SchedulingReason,
// and writes the per-node index entry for the target so the reservation (Task 7)
// and the heartbeat placement gate (Task 6) see the bound target. PinnedNodeID is
// untouched - the VM stays on source until cutover (spec D3). It is the seam the
// worker calls after SchedulePlacement returns a winner.
//
// Guards under a single ModRevision CAS: a terminal migration is rejected with
// store.ErrMigrationTerminal (binding a target on a dead saga would resurrect it);
// a migration already bound to a DIFFERENT target is rejected with
// store.ErrMigrationTargetConflict; re-binding the SAME target is an idempotent
// no-op (a redelivered worker that already bound). A lost CAS returns
// store.ErrConcurrentUpdate.
func (s *Store) BindMigrationTarget(ctx context.Context, migID, targetNodeID uuid.UUID, poolName string) error {
	resp, err := s.c.Raw().Get(ctx, migrationKey(migID))
	if err != nil {
		return err
	}
	if len(resp.Kvs) == 0 {
		return store.ErrNotFound
	}
	rev := resp.Kvs[0].ModRevision
	var m store.Migration
	if err := json.Unmarshal(resp.Kvs[0].Value, &m); err != nil {
		return err
	}
	if isTerminalMigration(m.Phase) {
		return store.ErrMigrationTerminal
	}
	if m.TargetNodeID != nil {
		if *m.TargetNodeID == targetNodeID {
			return nil // idempotent: already bound to this target.
		}
		return store.ErrMigrationTargetConflict
	}

	now := time.Now().UTC()
	target := targetNodeID
	m.TargetNodeID = &target
	m.TargetPoolName = poolName
	m.SchedulingReason = nil
	m.UpdatedAt = now

	val, err := etcd.Marshal(m)
	if err != nil {
		return err
	}
	ops := []clientv3.Op{
		clientv3.OpPut(migrationKey(m.ID), string(val)),
		clientv3.OpPut(migrationNodeIndexKey(target, m.ID), m.ID.String()),
	}

	txResp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(migrationKey(m.ID)), "=", rev)).
		Then(ops...).
		Commit()
	if err != nil {
		return err
	}
	if !txResp.Succeeded {
		return store.ErrConcurrentUpdate
	}
	return nil
}

// ActiveMigrationForVM returns the VM's single non-terminal migration and
// true, or (zero, false, nil) when none is active. It ranges the per-VM
// migration index (which retains terminal rows for history) and returns the
// first non-terminal one. A VM has at most one active migration - the
// CreateMigration active-per-VM guard enforces it - so "first" is unambiguous.
// Used by the lifecycle-precedence path (vm stop/delete cancels an active
// migration first) and the VM status.migration summary.
func (s *Store) ActiveMigrationForVM(ctx context.Context, vmID uuid.UUID) (store.Migration, bool, error) {
	items, err := s.c.Range(ctx, migrationsVMIndexPrefix(vmID))
	if err != nil {
		return store.Migration{}, false, err
	}
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return store.Migration{}, false, fmt.Errorf("corrupt migration vm index %q: %v", kv.Key, perr)
		}
		var m store.Migration
		found, gerr := s.c.GetJSON(ctx, migrationKey(id), &m)
		if gerr != nil {
			return store.Migration{}, false, gerr
		}
		if !found || isTerminalMigration(m.Phase) {
			continue
		}
		return m, true, nil
	}
	return store.Migration{}, false, nil
}

// MigrationByID returns the migration row with the given id, or
// store.ErrNotFound. Migrations have no soft-delete column, so a present row is
// always visible.
func (s *Store) MigrationByID(ctx context.Context, id uuid.UUID) (store.Migration, error) {
	var m store.Migration
	found, err := s.c.GetJSON(ctx, migrationKey(id), &m)
	if err != nil {
		return store.Migration{}, err
	}
	if !found {
		return store.Migration{}, store.ErrNotFound
	}
	return m, nil
}

// ListMigrations returns migrations newest-first by (created_at, id), applying
// the optional VM or node filter (VM takes precedence), then the cursor and
// LimitCount. With a node filter it returns migrations touching the node as
// source OR target (CreateMigration indexes both). It honors LimitCount as
// given - the handler passes LimitCount+1 for next-page detection.
func (s *Store) ListMigrations(ctx context.Context, p store.ListMigrationsParams) ([]store.Migration, error) {
	var migs []store.Migration
	var err error
	switch {
	case p.VmID != nil:
		migs, err = s.migrationsByIndex(ctx, migrationsVMIndexPrefix(*p.VmID))
	case p.NodeID != nil:
		migs, err = s.migrationsByIndex(ctx, migrationsNodeIndexPrefix(*p.NodeID))
	default:
		migs, err = s.migrationsByPrimaryPrefix(ctx)
	}
	if err != nil {
		return nil, err
	}

	out := make([]store.Migration, 0, len(migs))
	for _, m := range migs {
		if !beforeCursor(m.CreatedAt, m.ID, p.CursorCreatedAt, p.CursorID) {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	if n := int(p.LimitCount); n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// migrationsByIndex resolves each migration referenced by a secondary index
// prefix (per-VM or per-node), reading each primary. A dangling index entry
// (primary gone) is skipped.
func (s *Store) migrationsByIndex(ctx context.Context, prefix string) ([]store.Migration, error) {
	items, err := s.c.Range(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]store.Migration, 0, len(items))
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return nil, fmt.Errorf("corrupt migration index %q: %v", kv.Key, perr)
		}
		m, gerr := s.MigrationByID(ctx, id)
		if gerr != nil {
			if errors.Is(gerr, store.ErrNotFound) {
				continue
			}
			return nil, gerr
		}
		out = append(out, m)
	}
	return out, nil
}

// migrationsByPrimaryPrefix ranges every migration primary row directly (the
// unfiltered list path).
func (s *Store) migrationsByPrimaryPrefix(ctx context.Context) ([]store.Migration, error) {
	items, err := s.c.Range(ctx, etcd.Key("migrations")+"/")
	if err != nil {
		return nil, err
	}
	out := make([]store.Migration, 0, len(items))
	for _, kv := range items {
		var m store.Migration
		if err := json.Unmarshal(kv.Value, &m); err != nil {
			return nil, fmt.Errorf("unmarshal migration %q: %v", kv.Key, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// MigrationsByIDPrefix range-scans the migration primary keyspace for rows whose
// id begins with prefix (a lowercase hex string), decoding each match. It backs
// the get/cancel short-id resolver: the handler maps len 0 -> 404, len 1 ->
// resolved, len > 1 -> 409 ambiguous. The migration primary keyspace
// (/otherix/migrations/<id> -> row JSON) holds only row keys - the secondary
// indexes live under /otherix/index/migrations/... - so every key under the
// "migrations/<prefix>" range is a row and is decoded directly, mirroring
// migrationsByPrimaryPrefix. The scan caps at two matches: the caller only needs
// to distinguish 0 / 1 / many, so stopping at the second match bounds the work a
// broad (short) prefix can do.
func (s *Store) MigrationsByIDPrefix(ctx context.Context, prefix string) ([]store.Migration, error) {
	items, err := s.c.Range(ctx, etcd.Key("migrations")+"/"+prefix)
	if err != nil {
		return nil, err
	}
	out := make([]store.Migration, 0, 2)
	for _, kv := range items {
		var m store.Migration
		if err := json.Unmarshal(kv.Value, &m); err != nil {
			return nil, fmt.Errorf("unmarshal migration %q: %v", kv.Key, err)
		}
		out = append(out, m)
		if len(out) == 2 {
			break
		}
	}
	return out, nil
}
