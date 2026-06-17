// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"fmt"
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
