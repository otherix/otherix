// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package replication

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// DurabilityStore is the read surface desired-K derivation and the durability
// projection need. *etcdstore.Store satisfies it.
type DurabilityStore interface {
	SnapshotsReferencingBlob(ctx context.Context, digest string) ([]uuid.UUID, error)
	SnapshotByID(ctx context.Context, id uuid.UUID) (store.Snapshot, error)
	ArtifactPoolByName(ctx context.Context, name string) (store.ArtifactPool, error)
	BlobHolders(ctx context.Context, digest string) ([]uuid.UUID, error)
	AllNodes(ctx context.Context) ([]store.Node, error)
}

// Durability status values surfaced on a snapshot.
const (
	DurabilityUnknown     = "unknown"
	DurabilityDegraded    = "degraded"
	DurabilityReplicating = "replicating"
	DurabilityDurable     = "durable"
)

// blobPlacementTarget walks the snapshots referencing digest and returns the
// desired replica count K and the union of LIVE eligible member nodes across
// those snapshots' artifact pools. K is the strongest replication factor any
// referencing pool asks for: a concrete Count, or for the 'all' sentinel the
// count of live member nodes of that pool. A snapshot whose pool is unresolved
// (nil name or a deleted pool) contributes 1 and an empty member set, never
// raising K. The floor is 1.
func blobPlacementTarget(ctx context.Context, st DurabilityStore, digest string, nodes []store.Node, _ map[uuid.UUID]bool) (k int, eligible map[uuid.UUID]bool, err error) {
	snapIDs, err := st.SnapshotsReferencingBlob(ctx, digest)
	if err != nil {
		return 0, nil, err
	}
	k = 1
	eligible = map[uuid.UUID]bool{}
	for _, sid := range snapIDs {
		snap, err := st.SnapshotByID(ctx, sid)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, nil, err
		}
		if snap.ArtifactPoolName == nil {
			continue
		}
		pool, err := st.ArtifactPoolByName(ctx, *snap.ArtifactPoolName)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, nil, err
		}
		members := membershipNodeIDs(pool.Membership, nodes)
		for id := range members {
			eligible[id] = true
		}
		rf := int(pool.ReplicationFactor.Count)
		if pool.ReplicationFactor.All {
			rf = len(members)
		}
		if rf > k {
			k = rf
		}
	}
	return k, eligible, nil
}

// liveHolders returns the live node ids observed holding digest (BlobHolders
// intersected with the live-node set).
func liveHolders(ctx context.Context, st DurabilityStore, digest string, live map[uuid.UUID]bool) ([]uuid.UUID, error) {
	holders, err := st.BlobHolders(ctx, digest)
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(holders))
	for _, h := range holders {
		if live[h] {
			out = append(out, h)
		}
	}
	return out, nil
}

// classifyDigest classifies one digest. observed >= desired is durable; short of
// K with at least one holder AND a free eligible target can still progress
// (replicating); zero holders or no room to grow while short is degraded.
func classifyDigest(observed, desired, eligibleNotHolding int) string {
	switch {
	case observed >= desired:
		return DurabilityDurable
	case observed > 0 && eligibleNotHolding > 0:
		return DurabilityReplicating
	default:
		return DurabilityDegraded
	}
}

// weaker returns the less-durable of two per-digest statuses (degraded <
// replicating < durable).
func weaker(a, b string) string {
	if durabilityRank(b) < durabilityRank(a) {
		return b
	}
	return a
}

func durabilityRank(s string) int {
	switch s {
	case DurabilityDurable:
		return 3
	case DurabilityReplicating:
		return 2
	case DurabilityDegraded:
		return 1
	default:
		return 0
	}
}

// SnapshotDurability computes the durability projection for a snapshot: the
// weakest per-disk-digest status, the desired replica count (max over disks), and
// the observed replica count (min over disks). A snapshot with no disks yet (not
// produced) is unknown. Best-effort: the caller renders unknown / logs on error.
func SnapshotDurability(ctx context.Context, st DurabilityStore, snap store.Snapshot) (status string, desired, observed int, err error) {
	if len(snap.Disks) == 0 {
		return DurabilityUnknown, 0, 0, nil
	}
	nodes, err := st.AllNodes(ctx)
	if err != nil {
		return DurabilityUnknown, 0, 0, err
	}
	live := liveNodeIDs(nodes)
	status = DurabilityDurable
	minObserved := -1
	for _, d := range snap.Disks {
		k, eligible, err := blobPlacementTarget(ctx, st, d.SHA256, nodes, live)
		if err != nil {
			return DurabilityUnknown, 0, 0, err
		}
		holders, err := liveHolders(ctx, st, d.SHA256, live)
		if err != nil {
			return DurabilityUnknown, 0, 0, err
		}
		obs := len(holders)
		held := make(map[uuid.UUID]bool, len(holders))
		for _, h := range holders {
			held[h] = true
		}
		notHolding := 0
		for id := range eligible {
			if !held[id] {
				notHolding++
			}
		}
		status = weaker(status, classifyDigest(obs, k, notHolding))
		if k > desired {
			desired = k
		}
		if minObserved < 0 || obs < minObserved {
			minObserved = obs
		}
	}
	if minObserved < 0 {
		minObserved = 0
	}
	return status, desired, minObserved, nil
}
