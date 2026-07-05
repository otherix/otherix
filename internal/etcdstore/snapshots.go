// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// Snapshots are content-addressed, disk-only VM snapshots: a manifest (snapshot
// id + ordered per-disk blob digests) plus full-copy blobs. The CP holds the
// manifest, an owner index (feeding CountUserResources), a per-VM index, and a
// blob reference graph that is the authoritative, fail-closed input to blob GC.

func snapshotKey(id uuid.UUID) string { return etcd.Key("snapshots", id.String()) }

func snapshotPrefix() string { return etcd.Key("snapshots") + "/" }

// snapshotVMNameGuard enforces a unique snapshot name within a VM (non-deleted).
func snapshotVMNameGuard(vmID uuid.UUID, name string) string {
	return etcd.Key("uniq", "snapshots", "vm_name", vmID.String(), strings.ToLower(name))
}

// snapshotOwnerIndexKey is the owner-scoped index entry feeding CountUserResources.
func snapshotOwnerIndexKey(ownerID, id uuid.UUID) string {
	return etcd.Key("index", "snapshots", "owner", ownerID.String(), id.String())
}

func snapshotVMIndexKey(vmID, id uuid.UUID) string {
	return etcd.Key("index", "snapshots", "vm", vmID.String(), id.String())
}

func snapshotVMIndexPrefix(vmID uuid.UUID) string {
	return etcd.Key("index", "snapshots", "vm", vmID.String()) + "/"
}

// blobRefKey is a reference-graph entry recording that a snapshot references a
// blob digest. It is the authoritative input to fail-closed blob GC (a blob may
// be deleted only when no blobRef entries remain under its digest).
func blobRefKey(digest string, snapshotID uuid.UUID) string {
	return etcd.Key("index", "blob_refs", digest, snapshotID.String())
}

func blobRefPrefix(digest string) string {
	return etcd.Key("index", "blob_refs", digest) + "/"
}

// placementKey records that the blob with the given digest is held on a node. It
// is the K=1 seed of the §3/§5 placement map (replicated-artifact-store design):
// the snapshot-create success callback writes one entry per disk under the
// producing node, and blob GC reads it to find a blob's holder node(s) WITHOUT a
// VM lookup - so a snapshot stays deletable after its source VM is gone. Future
// replication grows the map from {1 node} to {K nodes} + a reconcile loop; the
// key schema and the GC's read are unchanged.
func placementKey(digest string, nodeID uuid.UUID) string {
	return etcd.Key("placement", digest, nodeID.String())
}

func placementPrefix(digest string) string {
	return etcd.Key("placement", digest) + "/"
}

// CreateSnapshot writes a creating snapshot row, its per-VM name guard, the
// owner and per-VM secondary indexes, and the backing task+job in one
// transaction (the direct analog of CreateMigration). The CAS on the name
// guard's CreateRevision==0 fails closed when the VM already has a non-deleted
// snapshot of the same name, returning store.ErrSnapshotNameExists. The manifest
// Disks list is empty at create time - the agent produces the blobs and the
// worker fills Disks (and writes the blobRefKey reference-graph entries) on
// success, when the digests are known.
func (s *Store) CreateSnapshot(ctx context.Context, p store.CreateSnapshotParams, args queue.JobArgs) (store.Snapshot, error) {
	now := time.Now().UTC()
	snap := store.Snapshot{
		ID:                 p.ID,
		VmID:               p.VmID,
		OwnerID:            p.OwnerID,
		Name:               p.Name,
		Description:        p.Description,
		Status:             store.SnapshotStatusCreating,
		WithMemory:         p.WithMemory,
		VMStateAtSnapshot:  p.VMStateAtSnapshot,
		SourceVMName:       p.SourceVMName,
		SourceArchitecture: p.SourceArchitecture,
		SourceFirmwareID:   p.SourceFirmwareID,
		ArtifactPoolName:   p.ArtifactPoolName,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	val, err := etcd.Marshal(snap)
	if err != nil {
		return store.Snapshot{}, err
	}
	seq, jobOp, err := s.enqueueJobOp(ctx, args)
	if err != nil {
		return store.Snapshot{}, err
	}
	task := taskFromParams(p.Task, seq)
	taskVal, err := etcd.Marshal(task)
	if err != nil {
		return store.Snapshot{}, err
	}

	guard := snapshotVMNameGuard(p.VmID, p.Name)
	ops := []clientv3.Op{
		clientv3.OpPut(guard, p.ID.String()),
		clientv3.OpPut(snapshotKey(snap.ID), string(val)),
		clientv3.OpPut(snapshotOwnerIndexKey(p.OwnerID, snap.ID), snap.ID.String()),
		clientv3.OpPut(snapshotVMIndexKey(p.VmID, snap.ID), snap.ID.String()),
		clientv3.OpPut(taskKey(task.ID), string(taskVal)),
		jobOp,
	}
	ops = append(ops, taskIndexOps(task)...)

	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(ops...).
		Commit()
	if err != nil {
		return store.Snapshot{}, fmt.Errorf("create snapshot txn: %v", err)
	}
	if !resp.Succeeded {
		return store.Snapshot{}, store.ErrSnapshotNameExists
	}
	return snap, nil
}

// SnapshotByID returns the snapshot row with the given id, or store.ErrNotFound
// when it is absent or soft-deleted.
func (s *Store) SnapshotByID(ctx context.Context, id uuid.UUID) (store.Snapshot, error) {
	var snap store.Snapshot
	found, err := s.c.GetJSON(ctx, snapshotKey(id), &snap)
	if err != nil {
		return store.Snapshot{}, err
	}
	if !found || snap.DeletedAt != nil {
		return store.Snapshot{}, store.ErrNotFound
	}
	return snap, nil
}

// ListSnapshots returns non-deleted snapshots newest-first by (created_at, id),
// applying every provided filter (VmID, OwnerID, Status) CONJUNCTIVELY, then the
// DESC cursor and LimitCount. The chosen index (per-VM if VmID set, else per-owner
// if OwnerID set, else the primary prefix) is only an iteration choice for
// efficiency: it never relaxes a filter. A row is kept only when it matches ALL
// set filters, so OwnerID still scopes a ScopeOwn caller even when VmID is also
// set (a developer cannot widen own-scope by passing another owner's VM id). It
// mirrors ListMigrations: the handler passes LimitCount+1 for next-page detection.
func (s *Store) ListSnapshots(ctx context.Context, p store.ListSnapshotsParams) ([]store.Snapshot, error) {
	var snaps []store.Snapshot
	var err error
	switch {
	case p.VmID != nil:
		snaps, err = s.snapshotsByIndex(ctx, snapshotVMIndexPrefix(*p.VmID))
	case p.OwnerID != nil:
		snaps, err = s.snapshotsByIndex(ctx, etcd.Key("index", "snapshots", "owner", p.OwnerID.String())+"/")
	default:
		snaps, err = s.snapshotsByPrimaryPrefix(ctx)
	}
	if err != nil {
		return nil, err
	}

	out := make([]store.Snapshot, 0, len(snaps))
	for _, snap := range snaps {
		if snap.DeletedAt != nil {
			continue
		}
		if !snapshotMatchesFilters(snap, p) {
			continue
		}
		if !beforeCursor(snap.CreatedAt, snap.ID, p.CursorCreatedAt, p.CursorID) {
			continue
		}
		out = append(out, snap)
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

// snapshotMatchesFilters ANDs every set filter (VmID, OwnerID, Status) against the
// row. The index chosen in ListSnapshots is only an iteration choice, so this
// predicate is what actually scopes the result: it keeps a ScopeOwn caller's
// OwnerID pin in force even when VmID is also set (no own-scope widening via ?vm).
func snapshotMatchesFilters(snap store.Snapshot, p store.ListSnapshotsParams) bool {
	if p.VmID != nil && snap.VmID != *p.VmID {
		return false
	}
	if p.OwnerID != nil && snap.OwnerID != *p.OwnerID {
		return false
	}
	if p.Status != nil && snap.Status != *p.Status {
		return false
	}
	return true
}

// snapshotsByIndex resolves each snapshot referenced by a secondary index prefix
// (per-VM or per-owner), reading each primary. A dangling index entry (primary
// gone) is skipped.
func (s *Store) snapshotsByIndex(ctx context.Context, prefix string) ([]store.Snapshot, error) {
	items, err := s.c.Range(ctx, prefix)
	if err != nil {
		return nil, err
	}
	out := make([]store.Snapshot, 0, len(items))
	for _, kv := range items {
		id, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return nil, fmt.Errorf("corrupt snapshot index %q: %v", kv.Key, perr)
		}
		snap, gerr := s.SnapshotByID(ctx, id)
		if gerr != nil {
			if errors.Is(gerr, store.ErrNotFound) {
				continue
			}
			return nil, gerr
		}
		out = append(out, snap)
	}
	return out, nil
}

// snapshotsByPrimaryPrefix ranges every snapshot primary row directly (the
// unfiltered list path). Soft-deleted rows are filtered by the caller.
func (s *Store) snapshotsByPrimaryPrefix(ctx context.Context) ([]store.Snapshot, error) {
	items, err := s.c.Range(ctx, snapshotPrefix())
	if err != nil {
		return nil, err
	}
	out := make([]store.Snapshot, 0, len(items))
	for _, kv := range items {
		var snap store.Snapshot
		if err := json.Unmarshal(kv.Value, &snap); err != nil {
			return nil, fmt.Errorf("unmarshal snapshot %q: %v", kv.Key, err)
		}
		out = append(out, snap)
	}
	return out, nil
}

// UpdateSnapshotMeta rewrites name and/or description, bumping updated_at. On a
// rename it moves the per-VM name guard in one transaction, gated on the new
// guard being free; a collision returns store.ErrSnapshotNameExists. A
// description-only change is a plain put. Nil fields are left unchanged.
func (s *Store) UpdateSnapshotMeta(ctx context.Context, p store.UpdateSnapshotMetaParams) (store.Snapshot, error) {
	existing, err := s.SnapshotByID(ctx, p.ID)
	if err != nil {
		return store.Snapshot{}, err
	}
	updated := existing
	if p.Description != nil {
		updated.Description = *p.Description
	}
	if p.Name != nil {
		updated.Name = *p.Name
	}
	updated.UpdatedAt = time.Now().UTC()

	val, err := etcd.Marshal(updated)
	if err != nil {
		return store.Snapshot{}, err
	}

	// Detect a true rename by guard inequality (the guard is lowercased), not by
	// a raw name compare: a case-only change ("daily" -> "Daily") yields the same
	// guard key, so it must take the plain-put branch rather than a guard-move
	// txn that would OpPut+OpDelete the same key (etcd rejects that as a duplicate
	// key). The plain put still persists the new display-case name.
	oldGuard := snapshotVMNameGuard(existing.VmID, existing.Name)
	newGuard := snapshotVMNameGuard(existing.VmID, updated.Name)
	if oldGuard == newGuard {
		if err := s.c.Put(ctx, snapshotKey(p.ID), val); err != nil {
			return store.Snapshot{}, err
		}
		return updated, nil
	}

	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(newGuard), "=", 0)).
		Then(
			clientv3.OpPut(newGuard, p.ID.String()),
			clientv3.OpDelete(oldGuard),
			clientv3.OpPut(snapshotKey(p.ID), string(val)),
		).
		Commit()
	if err != nil {
		return store.Snapshot{}, fmt.Errorf("update snapshot meta txn: %v", err)
	}
	if !resp.Succeeded {
		return store.Snapshot{}, store.ErrSnapshotNameExists
	}
	return updated, nil
}

// checkSnapshotDeletable runs the two fail-closed guards a soft-delete must pass:
// no live snapshot parents off this id (children check, via the per-VM index),
// and no active (pending|running) vm.create names this snapshot as its source
// (a sourcing create pulls the blobs without pinning them, so a delete-now could
// GC a blob it still needs). A store error on either read aborts the delete
// (never proceed on an uncertain read) - erring toward the non-destructive refusal.
func (s *Store) checkSnapshotDeletable(ctx context.Context, id, vmID uuid.UUID) error {
	siblings, err := s.snapshotsByIndex(ctx, snapshotVMIndexPrefix(vmID))
	if err != nil {
		return err
	}
	for _, sib := range siblings {
		if sib.DeletedAt == nil && sib.ParentSnapshotID != nil && *sib.ParentSnapshotID == id {
			return store.ErrSnapshotHasChildren
		}
	}
	activeCreates, err := s.ActiveCreatesReferencingSnapshot(ctx, id)
	if err != nil {
		return err
	}
	if activeCreates > 0 {
		return store.ErrSnapshotSourcingCreate
	}
	return nil
}

// DeleteSnapshot soft-deletes a snapshot fail-closed: it refuses with
// store.ErrSnapshotHasChildren when any non-deleted snapshot of the same VM
// names this snapshot as its parent, and with store.ErrSnapshotSourcingCreate
// when an active (pending|running) vm.create names this snapshot as its source,
// mutating nothing in either case (a delete must fail toward inaction so a
// still-referenced base, or a blob an in-flight create still needs, is never
// destroyed). When
// no children exist it stamps deleted_at + status=deleting, drops the per-VM
// name guard and the owner index (so CountUserResources falls), AND enqueues the
// blob-GC task+job - all in ONE transaction. Enqueuing the GC job atomically with
// the soft-delete (the same pattern CreateSnapshot uses) closes a crash window: a
// CP crash between a separate soft-delete and a separate enqueue would leave a
// soft-deleted row whose blobs never get a reclaimer (the retried DELETE 404s on
// the already-deleted row). It does NOT delete the disk blobs or their blobRef
// entries - that is the enqueued GC task's job, gated on the reference graph. The
// soft-delete put is CAS-guarded on the row's ModRevision so it never interleaves
// with a concurrent SnapshotManifestApplied projection (torn state); a lost CAS
// re-reads and retries, returning store.ErrConcurrentUpdate only if it never wins.
func (s *Store) DeleteSnapshot(ctx context.Context, id uuid.UUID, taskParams store.CreateTaskParams, args queue.JobArgs) (store.Snapshot, error) {
	// Allocate the GC task + job ONCE (a fixed task id + a single job seq), so a
	// CAS retry below reuses them idempotently rather than leaking a task/seq per
	// attempt. The job seq counter advances on allocation; reusing the same ops
	// keeps at most one seq consumed regardless of retries.
	seq, jobOp, err := s.enqueueJobOp(ctx, args)
	if err != nil {
		return store.Snapshot{}, err
	}
	task := taskFromParams(taskParams, seq)
	taskVal, err := etcd.Marshal(task)
	if err != nil {
		return store.Snapshot{}, err
	}

	const maxAttempts = 3
	for attempt := 0; ; attempt++ {
		// Read the row + its ModRevision so the soft-delete CAS-guards its blind put.
		// Without the CAS, this delete's put (deleting + dropped guards + enqueued GC)
		// can interleave with a concurrent SnapshotManifestApplied (ready + refgraph),
		// leaving a torn row (guardless "ready" with GC scheduled, or a resurrected
		// snapshot).
		resp, err := s.c.Raw().Get(ctx, snapshotKey(id))
		if err != nil {
			return store.Snapshot{}, fmt.Errorf("read snapshot %s: %v", id, err)
		}
		if len(resp.Kvs) == 0 {
			return store.Snapshot{}, store.ErrNotFound
		}
		rev := resp.Kvs[0].ModRevision
		var existing store.Snapshot
		if err := json.Unmarshal(resp.Kvs[0].Value, &existing); err != nil {
			return store.Snapshot{}, fmt.Errorf("unmarshal snapshot %s: %v", id, err)
		}
		if existing.DeletedAt != nil {
			// Already soft-deleted (a concurrent delete won): report not-found rather
			// than double-deleting / re-enqueuing GC.
			return store.Snapshot{}, store.ErrNotFound
		}

		if err := s.checkSnapshotDeletable(ctx, id, existing.VmID); err != nil {
			return store.Snapshot{}, err
		}

		now := time.Now().UTC()
		existing.DeletedAt = &now
		existing.Status = store.SnapshotStatusDeleting
		existing.UpdatedAt = now
		val, err := etcd.Marshal(existing)
		if err != nil {
			return store.Snapshot{}, err
		}

		ops := []clientv3.Op{
			clientv3.OpPut(snapshotKey(id), string(val)),
			clientv3.OpDelete(snapshotVMNameGuard(existing.VmID, existing.Name)),
			clientv3.OpDelete(snapshotOwnerIndexKey(existing.OwnerID, id)),
			clientv3.OpPut(taskKey(task.ID), string(taskVal)),
			jobOp,
		}
		ops = append(ops, taskIndexOps(task)...)

		txnResp, err := s.c.Raw().Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(snapshotKey(id)), "=", rev)).
			Then(ops...).
			Commit()
		if err != nil {
			return store.Snapshot{}, fmt.Errorf("delete snapshot txn: %v", err)
		}
		if txnResp.Succeeded {
			return existing, nil
		}
		// CAS lost: the row moved between read and commit (a concurrent projection or
		// delete). Re-read, re-check, retry a bounded number of times.
		if attempt+1 >= maxAttempts {
			return store.Snapshot{}, store.ErrConcurrentUpdate
		}
	}
}

// SnapshotByIDIncludingDeleted returns the snapshot row with the given id even
// when it is soft-deleted, or store.ErrNotFound only when the key is absent. The
// blob-GC worker runs AFTER DeleteSnapshot soft-deletes the row, so it reads the
// manifest digests off the deleted row through this method (SnapshotByID would
// return ErrNotFound on a soft-deleted snapshot).
func (s *Store) SnapshotByIDIncludingDeleted(ctx context.Context, id uuid.UUID) (store.Snapshot, error) {
	var snap store.Snapshot
	found, err := s.c.GetJSON(ctx, snapshotKey(id), &snap)
	if err != nil {
		return store.Snapshot{}, err
	}
	if !found {
		return store.Snapshot{}, store.ErrNotFound
	}
	return snap, nil
}

// SnapshotManifestApplied is the worker's success callback: it fills the
// manifest Disks, sets vm_state_at_snapshot from the agent report (the agent is
// authoritative for what was actually captured), flips status to ready, bumps
// updated_at, writes one blobRefKey reference-graph entry per disk (value = the
// snapshot id), and seeds the placement map with one placementKey entry per disk
// under nodeID (the node that produced the blobs) - all in one transaction. The
// refgraph entries are the authoritative, fail-closed input to blob GC (a blob may
// be deleted only when no blobRef entries remain under its digest). The placement
// entries let blob GC find a blob's holder node WITHOUT a VM lookup, so the
// snapshot stays deletable after its source VM is gone.
func (s *Store) SnapshotManifestApplied(ctx context.Context, id, nodeID uuid.UUID, disks []store.SnapshotDisk, vmState store.VMStateAtSnapshot) error {
	const maxAttempts = 3
	for attempt := 0; ; attempt++ {
		// Read the row + its ModRevision so the projection CAS-guards its put. A
		// blind put here can resurrect a row a concurrent DeleteSnapshot soft-deleted
		// between this read and this commit (the old code read via SnapshotByID and
		// blind-put the pre-delete value back, clearing DeletedAt) - a guardless
		// "ready" snapshot whose owner index was dropped and whose blob GC is already
		// enqueued (torn durable state / blob leak).
		resp, err := s.c.Raw().Get(ctx, snapshotKey(id))
		if err != nil {
			return fmt.Errorf("read snapshot %s: %v", id, err)
		}
		if len(resp.Kvs) == 0 {
			// The row is gone entirely: nothing to project onto, and we must not
			// re-create it. Treat as a no-op.
			return nil
		}
		rev := resp.Kvs[0].ModRevision
		var existing store.Snapshot
		if err := json.Unmarshal(resp.Kvs[0].Value, &existing); err != nil {
			return fmt.Errorf("unmarshal snapshot %s: %v", id, err)
		}
		if existing.DeletedAt != nil {
			// A DeleteSnapshot won the race and soft-deleted the row: do NOT resurrect
			// it to ready. The delete's enqueued blob GC reclaims what the agent
			// created; this create has nothing durable left to project.
			return nil
		}

		existing.Disks = disks
		existing.VMStateAtSnapshot = vmState
		existing.Status = store.SnapshotStatusReady
		existing.UpdatedAt = time.Now().UTC()
		val, err := etcd.Marshal(existing)
		if err != nil {
			return err
		}

		// 1 row put + 2 ops per disk (blobRef + placement). The current path is single-disk (3
		// ops); a future multi-disk path must chunk via commitInChunks before it nears
		// etcd's 128-op/txn limit (~63 disks), which no real VM approaches.
		ops := []clientv3.Op{clientv3.OpPut(snapshotKey(id), string(val))}
		for _, d := range disks {
			ops = append(ops, clientv3.OpPut(blobRefKey(d.SHA256, id), id.String()))
			ops = append(ops, clientv3.OpPut(placementKey(d.SHA256, nodeID), nodeID.String()))
		}
		txnResp, err := s.c.Raw().Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(snapshotKey(id)), "=", rev)).
			Then(ops...).
			Commit()
		if err != nil {
			return fmt.Errorf("snapshot manifest applied txn: %v", err)
		}
		if txnResp.Succeeded {
			return nil
		}
		// CAS lost: the row moved between read and commit. Re-read and retry a bounded
		// number of times (the next read observes a soft-delete and aborts, or the
		// fresh revision commits).
		if attempt+1 >= maxAttempts {
			return store.ErrConcurrentUpdate
		}
	}
}

// maxSnapshotErrorMessage bounds the error_message stamped on a snapshot row so a
// large agent error string cannot bloat the persisted row.
const maxSnapshotErrorMessage = 1024

// MarkSnapshotError surfaces a capture failure onto the snapshot row: it flips a
// still-creating row to status=error and sets error_message (capped) in a single
// transaction, so an operator listing snapshots sees the reason rather than a
// permanently-creating row. It is a best-effort operator nicety - the backing task
// failure is the authoritative signal - and is deliberately narrow: it transitions
// ONLY a creating row. It is a no-op (returns nil) when the row is absent, already
// soft-deleted, or already terminal (ready/error/restoring/deleting), so it never
// resurrects a deleted row and never clobbers a committed success on a redelivered
// failure. The msg is capped at maxSnapshotErrorMessage runes.
func (s *Store) MarkSnapshotError(ctx context.Context, id uuid.UUID, msg string) error {
	resp, err := s.c.Raw().Get(ctx, snapshotKey(id))
	if err != nil {
		return fmt.Errorf("read snapshot %s: %v", id, err)
	}
	if len(resp.Kvs) == 0 {
		return nil
	}
	rev := resp.Kvs[0].ModRevision
	var snap store.Snapshot
	if err := json.Unmarshal(resp.Kvs[0].Value, &snap); err != nil {
		return fmt.Errorf("unmarshal snapshot %s: %v", id, err)
	}
	// Only a still-creating, non-deleted row transitions. A soft-deleted row, or one
	// that already reached any other status, is left untouched (no resurrection, no
	// clobber of a committed success).
	if snap.DeletedAt != nil || snap.Status != store.SnapshotStatusCreating {
		return nil
	}

	capped := msg
	if len([]rune(capped)) > maxSnapshotErrorMessage {
		capped = string([]rune(capped)[:maxSnapshotErrorMessage])
	}
	snap.Status = store.SnapshotStatusError
	snap.ErrorMessage = &capped
	snap.UpdatedAt = time.Now().UTC()

	val, err := etcd.Marshal(snap)
	if err != nil {
		return err
	}
	// CAS on ModRevision so a concurrent success projection (SnapshotManifestApplied)
	// or delete that committed between our read and write wins - we must not overwrite
	// it. A lost CAS means the row already moved on; treat it as a benign no-op.
	if _, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(snapshotKey(id)), "=", rev)).
		Then(clientv3.OpPut(snapshotKey(id), string(val))).
		Commit(); err != nil {
		return fmt.Errorf("mark snapshot error txn: %v", err)
	}
	return nil
}

// BlobPlacements returns the node ids holding the blob with the given digest, read
// from the placement map (the K=1 seed of the §3/§5 design). Blob GC uses it to
// resolve a snapshot's holder node(s) WITHOUT loading the source VM, so deletion
// works after the VM is gone. The order is unspecified.
//
// Placement entries are NOT pruned by the best-effort delete path: a fire-and-forget
// agent delete cannot confirm a blob is physically gone, so removing the entry on an
// unconfirmed "success" could discard the only VM-independent pointer to a blob the
// agent fail-closed-leaked. The map is therefore a durable, conservative reclamation
// index; pruning it (and reclaiming leaked blobs) is the blob-reconcile loop's
// job, which can actually verify blob presence per node.
func (s *Store) BlobPlacements(ctx context.Context, digest string) ([]uuid.UUID, error) {
	items, err := s.c.Range(ctx, placementPrefix(digest))
	if err != nil {
		return nil, fmt.Errorf("read placement %q: %v", digest, err)
	}
	out := make([]uuid.UUID, 0, len(items))
	for _, kv := range items {
		nodeID, perr := uuid.Parse(string(kv.Value))
		if perr != nil {
			return nil, fmt.Errorf("corrupt placement entry %q: %v", kv.Key, perr)
		}
		out = append(out, nodeID)
	}
	return out, nil
}

// DereferenceSnapshotBlobs removes the snapshot's reference-graph entries for the
// given digests and returns ONLY the digests left with zero remaining references
// (the orphaned set safe to GC). It is the authoritative, fail-closed input to
// blob GC: for each digest it deletes blobRefKey(digest, snapshotID) and then reads
// blobRefPrefix(digest); a digest whose only remaining ref is this snapshot's
// entry (now deleted) is orphaned, while a digest still referenced by ANY other
// snapshot is never returned. The orphan decision reads the authoritative refgraph
// AFTER the delete commits, so a blob shared by content-addressed dedup is never
// handed to the agent for removal.
func (s *Store) DereferenceSnapshotBlobs(ctx context.Context, snapshotID uuid.UUID, digests []string) ([]string, error) {
	var orphaned []string
	for _, digest := range digests {
		// Delete this snapshot's ref AND read the remaining refs in ONE transaction so
		// the orphan decision is linearizable: the OpGet executes after the OpDelete at
		// the same revision, and a concurrent sibling delete of a shared (dedup'd) blob
		// either commits before this txn (its ref is gone from our read) or after (its
		// ref is still present, so we keep the blob). A delete-then-separate-range would
		// let two racing deletes both observe zero refs; the single txn forbids that.
		resp, err := s.c.Raw().Txn(ctx).
			Then(
				clientv3.OpDelete(blobRefKey(digest, snapshotID)),
				clientv3.OpGet(blobRefPrefix(digest), clientv3.WithPrefix()),
			).
			Commit()
		if err != nil {
			return nil, fmt.Errorf("dereference blob %q: %v", digest, err)
		}
		// Responses[1] is the post-delete range: zero entries means no other snapshot
		// references the blob, so it is orphaned and safe to GC. Any remaining entry is
		// another snapshot still holding the ref (fail-closed - the blob stays).
		if rng := resp.Responses[1].GetResponseRange(); rng == nil || len(rng.Kvs) == 0 {
			orphaned = append(orphaned, digest)
		}
	}
	return orphaned, nil
}

// CountSnapshotsInArtifactPool counts non-deleted snapshots tagged with the
// given artifact pool name (case-insensitive). The authoritative input to
// fail-closed artifact-pool delete. Scans the snapshot primary prefix (the
// artifact-pool delete is a rare admin op; no dedicated index).
func (s *Store) CountSnapshotsInArtifactPool(ctx context.Context, name string) (int64, error) {
	snaps, err := s.snapshotsByPrimaryPrefix(ctx)
	if err != nil {
		return 0, err
	}
	var count int64
	for _, snap := range snaps {
		if snap.DeletedAt != nil {
			continue
		}
		if snap.ArtifactPoolName != nil && strings.EqualFold(*snap.ArtifactPoolName, name) {
			count++
		}
	}
	return count, nil
}
