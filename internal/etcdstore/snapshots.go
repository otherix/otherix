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
		ID:                p.ID,
		VmID:              p.VmID,
		OwnerID:           p.OwnerID,
		Name:              p.Name,
		Description:       p.Description,
		Status:            store.SnapshotStatusCreating,
		WithMemory:        p.WithMemory,
		VMStateAtSnapshot: p.VMStateAtSnapshot,
		CreatedAt:         now,
		UpdatedAt:         now,
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
// applying the optional VM or owner filter (VM takes precedence), an optional
// Status filter, then the DESC cursor and LimitCount. It mirrors ListMigrations:
// the handler passes LimitCount+1 for next-page detection.
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
		if p.Status != nil && snap.Status != *p.Status {
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

// DeleteSnapshot soft-deletes a snapshot fail-closed: it refuses with
// store.ErrSnapshotHasChildren when any non-deleted snapshot of the same VM
// names this snapshot as its parent, mutating nothing in that case (a delete
// must fail toward inaction so a still-referenced base is never destroyed). When
// no children exist it stamps deleted_at + status=deleting, drops the per-VM
// name guard and the owner index (so CountUserResources falls) in one
// transaction. It does NOT delete the disk blobs or their blobRef entries - blob
// GC is the agent's job later, gated on the reference graph remaining empty.
func (s *Store) DeleteSnapshot(ctx context.Context, id uuid.UUID) (store.Snapshot, error) {
	existing, err := s.SnapshotByID(ctx, id)
	if err != nil {
		return store.Snapshot{}, err
	}

	// Fail-closed children check: scan the authoritative per-VM index and resolve
	// each sibling to its primary; refuse if any live snapshot parents off this id.
	siblings, err := s.snapshotsByIndex(ctx, snapshotVMIndexPrefix(existing.VmID))
	if err != nil {
		return store.Snapshot{}, err
	}
	for _, sib := range siblings {
		if sib.DeletedAt == nil && sib.ParentSnapshotID != nil && *sib.ParentSnapshotID == id {
			return store.Snapshot{}, store.ErrSnapshotHasChildren
		}
	}

	now := time.Now().UTC()
	existing.DeletedAt = &now
	existing.Status = store.SnapshotStatusDeleting
	existing.UpdatedAt = now
	val, err := etcd.Marshal(existing)
	if err != nil {
		return store.Snapshot{}, err
	}

	if _, err := s.c.Raw().Txn(ctx).
		Then(
			clientv3.OpPut(snapshotKey(id), string(val)),
			clientv3.OpDelete(snapshotVMNameGuard(existing.VmID, existing.Name)),
			clientv3.OpDelete(snapshotOwnerIndexKey(existing.OwnerID, id)),
		).
		Commit(); err != nil {
		return store.Snapshot{}, fmt.Errorf("delete snapshot txn: %v", err)
	}
	return existing, nil
}

// SnapshotManifestApplied is the worker's success callback: it fills the
// manifest Disks, flips status to ready, bumps updated_at, and writes one
// blobRefKey reference-graph entry per disk (value = the snapshot id) - all in
// one transaction. The refgraph entries are the authoritative, fail-closed input
// to blob GC: a future GC may delete a blob only when no blobRef entries remain
// under its digest, so this write is the seam that keeps GC from deleting a
// still-referenced blob.
func (s *Store) SnapshotManifestApplied(ctx context.Context, id uuid.UUID, disks []store.SnapshotDisk) error {
	existing, err := s.SnapshotByID(ctx, id)
	if err != nil {
		return err
	}
	existing.Disks = disks
	existing.Status = store.SnapshotStatusReady
	existing.UpdatedAt = time.Now().UTC()
	val, err := etcd.Marshal(existing)
	if err != nil {
		return err
	}

	ops := []clientv3.Op{clientv3.OpPut(snapshotKey(id), string(val))}
	for _, d := range disks {
		ops = append(ops, clientv3.OpPut(blobRefKey(d.SHA256, id), id.String()))
	}
	if _, err := s.c.Raw().Txn(ctx).Then(ops...).Commit(); err != nil {
		return fmt.Errorf("snapshot manifest applied txn: %v", err)
	}
	return nil
}
