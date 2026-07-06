// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package replication

import (
	"context"
	"errors"
	"sort"

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
//
// The blank live-node-set parameter is a stable seam for the reconcile planner,
// which shares this signature; the body derives liveness from nodes directly.
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

// DurabilityResolver computes the durability projection for many snapshots across
// one list page while touching etcd a bounded number of times. It fetches the
// node inventory once at construction and memoizes, keyed by blob digest, the
// per-digest placement target (desired K + eligible members) and the live-holder
// set, plus the SnapshotByID / ArtifactPoolByName lookups those derivations make.
// A page of N snapshots that share a digest collapses to one reference-index scan
// and one node-blob scan instead of N.
//
// Reuse a single resolver for a whole list page (call NewDurabilityResolver once,
// then Resolve per row). SnapshotDurability builds a one-shot resolver for a
// single read. A resolver is NOT safe for concurrent use: its memo maps are
// unsynchronised and a page is projected sequentially. The cached node set is a
// within-page-consistent liveness snapshot, which the durability projection
// already treats as best-effort.
type DurabilityResolver struct {
	st       DurabilityStore
	nodes    []store.Node
	live     map[uuid.UUID]bool
	nameByID map[uuid.UUID]string

	targetCache map[string]placementTarget     // digest -> (K, eligible)
	holderCache map[string][]uuid.UUID         // digest -> live holders
	snapCache   map[uuid.UUID]*store.Snapshot  // id -> snapshot (nil = not found)
	poolCache   map[string]*store.ArtifactPool // name -> pool (nil = not found)
}

// placementTarget is a memoized blobPlacementTarget result for one digest.
type placementTarget struct {
	k        int
	eligible map[uuid.UUID]bool
}

// NewDurabilityResolver fetches the node inventory once and returns a resolver
// ready to project a page of snapshots. The single AllNodes scan is the cost the
// per-row list path was paying N times.
func NewDurabilityResolver(ctx context.Context, st DurabilityStore) (*DurabilityResolver, error) {
	nodes, err := st.AllNodes(ctx)
	if err != nil {
		return nil, err
	}
	nameByID := make(map[uuid.UUID]string, len(nodes))
	for _, n := range nodes {
		nameByID[n.ID] = n.Name
	}
	return &DurabilityResolver{
		st:          st,
		nodes:       nodes,
		live:        liveNodeIDs(nodes),
		nameByID:    nameByID,
		targetCache: map[string]placementTarget{},
		holderCache: map[string][]uuid.UUID{},
		snapCache:   map[uuid.UUID]*store.Snapshot{},
		poolCache:   map[string]*store.ArtifactPool{},
	}, nil
}

// Resolve computes the durability projection for one snapshot against the
// resolver's cached node set, reproducing SnapshotDurability's semantics: the
// weakest per-disk-digest status, the desired replica count (max over disks), the
// observed replica count (min over disks), and holderNodes - the sorted unique
// names of the live nodes currently holding the snapshot's blobs (the union of
// live holders across the manifest disks). A snapshot with no disks yet (not
// produced) is unknown with nil holderNodes.
func (r *DurabilityResolver) Resolve(ctx context.Context, snap store.Snapshot) (status string, desired, observed int, holderNodes []string, err error) {
	if len(snap.Disks) == 0 {
		return DurabilityUnknown, 0, 0, nil, nil
	}
	status = DurabilityDurable
	minObserved := -1
	holderNames := map[string]bool{}
	for _, d := range snap.Disks {
		k, eligible, err := r.target(ctx, d.SHA256)
		if err != nil {
			return DurabilityUnknown, 0, 0, nil, err
		}
		holders, err := r.holders(ctx, d.SHA256)
		if err != nil {
			return DurabilityUnknown, 0, 0, nil, err
		}
		obs := len(holders)
		held := collectHolderNames(holders, r.nameByID, holderNames)
		status = weaker(status, classifyDigest(obs, k, eligibleNotHolding(eligible, held)))
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
	return status, desired, minObserved, sortedKeys(holderNames), nil
}

// target is the memoized per-digest placement target: desired K and the union of
// live eligible member nodes, reproducing blobPlacementTarget against the cached
// node set. It memoizes the SnapshotByID / ArtifactPoolByName lookups so a digest
// referenced by several snapshots, or several digests referencing one snapshot or
// pool, each hit the store once.
func (r *DurabilityResolver) target(ctx context.Context, digest string) (int, map[uuid.UUID]bool, error) {
	if t, ok := r.targetCache[digest]; ok {
		return t.k, t.eligible, nil
	}
	snapIDs, err := r.st.SnapshotsReferencingBlob(ctx, digest)
	if err != nil {
		return 0, nil, err
	}
	k := 1
	eligible := map[uuid.UUID]bool{}
	for _, sid := range snapIDs {
		snap, ok, err := r.snapshotByID(ctx, sid)
		if err != nil {
			return 0, nil, err
		}
		if !ok || snap.ArtifactPoolName == nil {
			continue
		}
		pool, ok, err := r.poolByName(ctx, *snap.ArtifactPoolName)
		if err != nil {
			return 0, nil, err
		}
		if !ok {
			continue
		}
		members := membershipNodeIDs(pool.Membership, r.nodes)
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
	r.targetCache[digest] = placementTarget{k: k, eligible: eligible}
	return k, eligible, nil
}

// holders is the memoized per-digest live-holder set (BlobHolders intersected with
// the cached live-node set).
func (r *DurabilityResolver) holders(ctx context.Context, digest string) ([]uuid.UUID, error) {
	if h, ok := r.holderCache[digest]; ok {
		return h, nil
	}
	all, err := r.st.BlobHolders(ctx, digest)
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(all))
	for _, h := range all {
		if r.live[h] {
			out = append(out, h)
		}
	}
	r.holderCache[digest] = out
	return out, nil
}

// snapshotByID memoizes a SnapshotByID lookup. The bool is false for a not-found
// row (cached as a nil sentinel); a transient (non-ErrNotFound) error is returned
// uncached.
func (r *DurabilityResolver) snapshotByID(ctx context.Context, id uuid.UUID) (store.Snapshot, bool, error) {
	if s, ok := r.snapCache[id]; ok {
		if s == nil {
			return store.Snapshot{}, false, nil
		}
		return *s, true, nil
	}
	s, err := r.st.SnapshotByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		r.snapCache[id] = nil
		return store.Snapshot{}, false, nil
	}
	if err != nil {
		return store.Snapshot{}, false, err
	}
	sc := s
	r.snapCache[id] = &sc
	return s, true, nil
}

// poolByName memoizes an ArtifactPoolByName lookup, with the same not-found /
// transient-error contract as snapshotByID.
func (r *DurabilityResolver) poolByName(ctx context.Context, name string) (store.ArtifactPool, bool, error) {
	if p, ok := r.poolCache[name]; ok {
		if p == nil {
			return store.ArtifactPool{}, false, nil
		}
		return *p, true, nil
	}
	p, err := r.st.ArtifactPoolByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		r.poolCache[name] = nil
		return store.ArtifactPool{}, false, nil
	}
	if err != nil {
		return store.ArtifactPool{}, false, err
	}
	pc := p
	r.poolCache[name] = &pc
	return p, true, nil
}

// SnapshotDurability computes the durability projection for a single snapshot. It
// is a thin wrapper over a one-shot DurabilityResolver, so its output is identical
// to the resolver-based list path. A snapshot with no disks yet (not produced) is
// unknown with nil holderNodes, resolved without touching the node inventory.
// Best-effort: the caller renders unknown / logs on error.
//
// Cost: one node-inventory scan plus, per disk, one reference-index and one
// node-blob scan. Fine for a single read; a list path must build ONE resolver per
// page and call Resolve per row rather than calling this per row.
func SnapshotDurability(ctx context.Context, st DurabilityStore, snap store.Snapshot) (status string, desired, observed int, holderNodes []string, err error) {
	if len(snap.Disks) == 0 {
		return DurabilityUnknown, 0, 0, nil, nil
	}
	r, err := NewDurabilityResolver(ctx, st)
	if err != nil {
		return DurabilityUnknown, 0, 0, nil, err
	}
	return r.Resolve(ctx, snap)
}

// collectHolderNames records the names of holders into names (a shared set
// across disks) and returns the held-id set for this disk.
func collectHolderNames(holders []uuid.UUID, nameByID map[uuid.UUID]string, names map[string]bool) map[uuid.UUID]bool {
	held := make(map[uuid.UUID]bool, len(holders))
	for _, h := range holders {
		held[h] = true
		if name := nameByID[h]; name != "" {
			names[name] = true
		}
	}
	return held
}

// eligibleNotHolding counts eligible nodes that are not already holding the blob.
func eligibleNotHolding(eligible, held map[uuid.UUID]bool) int {
	n := 0
	for id := range eligible {
		if !held[id] {
			n++
		}
	}
	return n
}

// sortedKeys returns the set keys sorted; nil for an empty set.
func sortedKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
