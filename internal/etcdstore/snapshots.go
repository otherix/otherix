// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"strings"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
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
