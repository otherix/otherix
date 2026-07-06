// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package replication

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

type inflightKey struct {
	digest string
	node   uuid.UUID
}

// reconcileStoreFake extends the durability fake with placement + enqueue.
type reconcileStoreFake struct {
	durabilityStoreFake
	placement map[string]map[uuid.UUID]bool // digest -> member set
	enqueued  []ReplicateArgs
	reclaimed []ReclaimArgs
	added     []struct {
		digest string
		node   uuid.UUID
	}
	removed []struct {
		digest string
		node   uuid.UUID
	}
	inflight        map[inflightKey]bool
	reclaimInflight map[inflightKey]bool
	nodeBlobDigests []string    // digests observed across node_blobs inventories
	nodeInvPrunes   []uuid.UUID // node ids whose observed inventory was pruned (UpsertNodeBlobInventory with a nil slice)
}

func (f *reconcileStoreFake) UpsertNodeBlobInventory(_ context.Context, nodeID uuid.UUID, blobs []store.NodeBlob) error {
	if blobs == nil {
		f.nodeInvPrunes = append(f.nodeInvPrunes, nodeID)
	}
	return nil
}

func (f *reconcileStoreFake) AllPlacementDigests(context.Context) ([]string, error) {
	out := make([]string, 0, len(f.placement))
	for d := range f.placement {
		out = append(out, d)
	}
	return out, nil
}

func (f *reconcileStoreFake) AllNodeBlobDigests(context.Context) ([]string, error) {
	return f.nodeBlobDigests, nil
}

func (f *reconcileStoreFake) BlobPlacements(_ context.Context, d string) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for n := range f.placement[d] {
		out = append(out, n)
	}
	return out, nil
}

func (f *reconcileStoreFake) AddBlobPlacement(_ context.Context, d string, n uuid.UUID) error {
	if f.placement[d] == nil {
		f.placement[d] = map[uuid.UUID]bool{}
	}
	f.placement[d][n] = true
	f.added = append(f.added, struct {
		digest string
		node   uuid.UUID
	}{d, n})
	return nil
}

func (f *reconcileStoreFake) RemoveBlobPlacement(_ context.Context, d string, n uuid.UUID) (bool, error) {
	_, ok := f.placement[d][n]
	delete(f.placement[d], n)
	f.removed = append(f.removed, struct {
		digest string
		node   uuid.UUID
	}{d, n})
	return ok, nil
}

func (f *reconcileStoreFake) EnqueueTask(_ context.Context, _ store.CreateTaskParams, args queue.JobArgs) (uuid.UUID, error) {
	switch a := args.(type) {
	case ReplicateArgs:
		f.enqueued = append(f.enqueued, a)
	case ReclaimArgs:
		f.reclaimed = append(f.reclaimed, a)
	default:
		panic("reconcileStoreFake: unexpected job args type")
	}
	return uuid.New(), nil
}

func (f *reconcileStoreFake) TryBeginReclaim(_ context.Context, d string, n uuid.UUID, _ time.Duration) (bool, error) {
	if f.reclaimInflight == nil {
		f.reclaimInflight = map[inflightKey]bool{}
	}
	k := inflightKey{d, n}
	if f.reclaimInflight[k] {
		return false, nil
	}
	f.reclaimInflight[k] = true
	return true, nil
}

func (f *reconcileStoreFake) EndReclaim(_ context.Context, d string, n uuid.UUID) error {
	delete(f.reclaimInflight, inflightKey{d, n})
	return nil
}

func (f *reconcileStoreFake) TryBeginReplicate(_ context.Context, d string, n uuid.UUID, _ time.Duration) (bool, error) {
	if f.inflight == nil {
		f.inflight = map[inflightKey]bool{}
	}
	k := inflightKey{d, n}
	if f.inflight[k] {
		return false, nil
	}
	f.inflight[k] = true
	return true, nil
}

func (f *reconcileStoreFake) EndReplicate(_ context.Context, d string, n uuid.UUID) error {
	delete(f.inflight, inflightKey{d, n})
	return nil
}

func TestReconcileAddsTargetAndEnqueuesToReachK(t *testing.T) {
	n1, n2, n3 := uuid.New(), uuid.New(), uuid.New()
	pool := "gold"
	digest := "d0"
	snapID := uuid.New()
	f := &reconcileStoreFake{
		durabilityStoreFake: durabilityStoreFake{
			refs:    map[string][]uuid.UUID{digest: {snapID}},
			snaps:   map[uuid.UUID]store.Snapshot{snapID: {ID: snapID, ArtifactPoolName: &pool}},
			pools:   map[string]store.ArtifactPool{pool: {Name: pool, ReplicationFactor: store.ReplicationFactor{Count: 2}, Membership: store.ArtifactPoolMembership{AllNodes: true}}},
			holders: map[string][]uuid.UUID{digest: {n1}},
			nodes: []store.Node{
				{ID: n1, Name: "node-1", Status: store.NodeStatusReady},
				{ID: n2, Name: "node-2", Status: store.NodeStatusReady},
				{ID: n3, Name: "node-3", Status: store.NodeStatusReady},
			},
		},
		placement: map[string]map[uuid.UUID]bool{digest: {n1: true}},
	}

	if err := ReconcileFunc(f, 5*time.Minute, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.added) != 1 {
		t.Fatalf("added %d placement members, want 1 (to reach K=2)", len(f.added))
	}
	if len(f.enqueued) != 1 {
		t.Fatalf("enqueued %d replicate tasks, want 1", len(f.enqueued))
	}
	if f.enqueued[0].Digest != digest || f.enqueued[0].TargetNodeID != f.added[0].node {
		t.Errorf("enqueued %+v, want digest=%s target=%s", f.enqueued[0], digest, f.added[0].node)
	}
}

func TestReconcilePrunesRebalanceEligibleMemberButNotLastPointer(t *testing.T) {
	dead1, holder2, n3 := uuid.New(), uuid.New(), uuid.New()
	pool := "gold"
	digest := "d0"
	snapID := uuid.New()
	stale := time.Now().UTC().Add(-1 * time.Hour) // older than the 5m grace below
	mk := func(holders []uuid.UUID, members []uuid.UUID) *reconcileStoreFake {
		pm := map[uuid.UUID]bool{}
		for _, m := range members {
			pm[m] = true
		}
		return &reconcileStoreFake{
			durabilityStoreFake: durabilityStoreFake{
				refs:    map[string][]uuid.UUID{digest: {snapID}},
				snaps:   map[uuid.UUID]store.Snapshot{snapID: {ID: snapID, ArtifactPoolName: &pool}},
				pools:   map[string]store.ArtifactPool{pool: {Name: pool, ReplicationFactor: store.ReplicationFactor{Count: 2}, Membership: store.ArtifactPoolMembership{AllNodes: true}}},
				holders: map[string][]uuid.UUID{digest: holders},
				nodes: []store.Node{
					{ID: dead1, Name: "node-1", Status: store.NodeStatusUnreachable, LastHeartbeatAt: &stale},
					{ID: holder2, Name: "node-2", Status: store.NodeStatusReady},
					{ID: n3, Name: "node-3", Status: store.NodeStatusReady},
				},
			},
			placement: map[string]map[uuid.UUID]bool{digest: pm},
		}
	}

	f := mk([]uuid.UUID{holder2}, []uuid.UUID{dead1, holder2})
	if err := ReconcileFunc(f, 5*time.Minute, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.removed) != 1 || f.removed[0].node != dead1 {
		t.Errorf("removed = %+v, want one prune of the rebalance-eligible member", f.removed)
	}

	f = mk([]uuid.UUID{}, []uuid.UUID{dead1})
	if err := ReconcileFunc(f, 5*time.Minute, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.removed) != 0 {
		t.Errorf("removed = %+v, want no prune of the last pointer", f.removed)
	}
}

// TestReconcilePrunesRebalanceEligibleInventory proves a rebalance-eligible node
// (unreachable with a heartbeat older than grace) has its observed blob inventory
// pruned once per pass and its placement member removed, while a still-fresh
// unreachable node is left untouched (reversible on a returning heartbeat).
func TestReconcilePrunesRebalanceEligibleInventory(t *testing.T) {
	dead, holder := uuid.New(), uuid.New()
	pool := "gold"
	digest := "d0"
	snapID := uuid.New()
	mk := func(hb time.Time) *reconcileStoreFake {
		return &reconcileStoreFake{
			durabilityStoreFake: durabilityStoreFake{
				refs:    map[string][]uuid.UUID{digest: {snapID}},
				snaps:   map[uuid.UUID]store.Snapshot{snapID: {ID: snapID, ArtifactPoolName: &pool}},
				pools:   map[string]store.ArtifactPool{pool: {Name: pool, ReplicationFactor: store.ReplicationFactor{Count: 1}, Membership: store.ArtifactPoolMembership{AllNodes: true}}},
				holders: map[string][]uuid.UUID{digest: {holder}},
				nodes: []store.Node{
					{ID: dead, Name: "node-1", Status: store.NodeStatusUnreachable, LastHeartbeatAt: &hb},
					{ID: holder, Name: "node-2", Status: store.NodeStatusReady},
				},
			},
			placement: map[string]map[uuid.UUID]bool{digest: {dead: true, holder: true}},
		}
	}

	stale := mk(time.Now().UTC().Add(-1 * time.Hour)) // older than the 5m grace below
	if err := ReconcileFunc(stale, 5*time.Minute, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile (stale): %v", err)
	}
	if len(stale.nodeInvPrunes) != 1 || stale.nodeInvPrunes[0] != dead {
		t.Errorf("nodeInvPrunes = %v, want one prune of the rebalance-eligible node %s", stale.nodeInvPrunes, dead)
	}
	if len(stale.removed) != 1 || stale.removed[0].node != dead {
		t.Errorf("removed = %+v, want the eligible node's placement member pruned", stale.removed)
	}

	fresh := mk(time.Now().UTC())
	if err := ReconcileFunc(fresh, 5*time.Minute, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile (fresh): %v", err)
	}
	if len(fresh.nodeInvPrunes) != 0 {
		t.Errorf("nodeInvPrunes = %v, want no prune while the node is still fresh", fresh.nodeInvPrunes)
	}
	if len(fresh.removed) != 0 {
		t.Errorf("removed = %+v, want no placement prune while the node is still fresh", fresh.removed)
	}
}

func TestReconcileNoLiveHolderSkipsEnqueue(t *testing.T) {
	n1, n2 := uuid.New(), uuid.New()
	pool := "gold"
	digest := "d0"
	snapID := uuid.New()
	f := &reconcileStoreFake{
		durabilityStoreFake: durabilityStoreFake{
			refs:    map[string][]uuid.UUID{digest: {snapID}},
			snaps:   map[uuid.UUID]store.Snapshot{snapID: {ID: snapID, ArtifactPoolName: &pool}},
			pools:   map[string]store.ArtifactPool{pool: {Name: pool, ReplicationFactor: store.ReplicationFactor{Count: 2}, Membership: store.ArtifactPoolMembership{AllNodes: true}}},
			holders: map[string][]uuid.UUID{digest: {}},
			nodes: []store.Node{
				{ID: n1, Name: "node-1", Status: store.NodeStatusReady},
				{ID: n2, Name: "node-2", Status: store.NodeStatusReady},
			},
		},
		placement: map[string]map[uuid.UUID]bool{digest: {n1: true}},
	}
	if err := ReconcileFunc(f, 5*time.Minute, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.enqueued) != 0 {
		t.Errorf("enqueued %d with no live holder, want 0 (fail toward inaction)", len(f.enqueued))
	}
}

// TestReconcileSkipsEnqueueWhileInflight proves the cross-pass dedup guard: when a
// replicate to a chosen target is already marked in-flight, the pass enqueues
// nothing for it; once the marker clears, a later pass enqueues exactly once.
func TestReconcileSkipsEnqueueWhileInflight(t *testing.T) {
	n1, n2 := uuid.New(), uuid.New()
	pool := "gold"
	digest := "d0"
	snapID := uuid.New()
	mk := func(inflight map[inflightKey]bool) *reconcileStoreFake {
		return &reconcileStoreFake{
			durabilityStoreFake: durabilityStoreFake{
				refs:    map[string][]uuid.UUID{digest: {snapID}},
				snaps:   map[uuid.UUID]store.Snapshot{snapID: {ID: snapID, ArtifactPoolName: &pool}},
				pools:   map[string]store.ArtifactPool{pool: {Name: pool, ReplicationFactor: store.ReplicationFactor{Count: 2}, Membership: store.ArtifactPoolMembership{AllNodes: true}}},
				holders: map[string][]uuid.UUID{digest: {n1}},
				nodes: []store.Node{
					{ID: n1, Name: "node-1", Status: store.NodeStatusReady},
					{ID: n2, Name: "node-2", Status: store.NodeStatusReady},
				},
			},
			// n1 holds, n2 is the chosen-but-not-yet-holding target.
			placement: map[string]map[uuid.UUID]bool{digest: {n1: true, n2: true}},
			inflight:  inflight,
		}
	}

	// First pass: n2's replicate is already in-flight -> enqueue nothing.
	f := mk(map[inflightKey]bool{{digest, n2}: true})
	if err := ReconcileFunc(f, 5*time.Minute, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile (in-flight): %v", err)
	}
	if len(f.enqueued) != 0 {
		t.Fatalf("enqueued %d while in-flight, want 0", len(f.enqueued))
	}

	// Clear the marker; a later pass enqueues exactly once.
	f = mk(map[inflightKey]bool{})
	if err := ReconcileFunc(f, 5*time.Minute, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile (cleared): %v", err)
	}
	if len(f.enqueued) != 1 || f.enqueued[0].TargetNodeID != n2 {
		t.Fatalf("enqueued = %+v, want exactly one for n2", f.enqueued)
	}
	if !f.inflight[inflightKey{digest, n2}] {
		t.Errorf("enqueue did not set the in-flight marker for n2")
	}
}

// TestReconcileOrphanedReclaimsAllHolders proves that an unreferenced digest
// (zero referencing snapshots) reclaims every live holder and never replicates.
func TestReconcileOrphanedReclaimsAllHolders(t *testing.T) {
	n1, n2 := uuid.New(), uuid.New()
	digest := "d0"
	f := &reconcileStoreFake{
		durabilityStoreFake: durabilityStoreFake{
			refs:    map[string][]uuid.UUID{digest: {}},
			holders: map[string][]uuid.UUID{digest: {n1, n2}},
			nodes: []store.Node{
				{ID: n1, Name: "node-1", Status: store.NodeStatusReady},
				{ID: n2, Name: "node-2", Status: store.NodeStatusReady},
			},
		},
		placement: map[string]map[uuid.UUID]bool{digest: {n1: true, n2: true}},
	}
	if err := ReconcileFunc(f, 5*time.Minute, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.reclaimed) != 2 {
		t.Fatalf("reclaimed %d holders, want 2 (orphaned)", len(f.reclaimed))
	}
	if len(f.enqueued) != 0 {
		t.Errorf("enqueued %d replicate tasks for an orphaned digest, want 0", len(f.enqueued))
	}
	got := map[uuid.UUID]bool{}
	for _, r := range f.reclaimed {
		if r.Digest != digest {
			t.Errorf("reclaim digest = %s, want %s", r.Digest, digest)
		}
		got[r.TargetNodeID] = true
	}
	if !got[n1] || !got[n2] {
		t.Errorf("reclaimed targets = %v, want both n1 and n2", got)
	}
}

// TestReconcileOrphanedPrunesObservedAbsentMember proves that an orphaned digest
// prunes the placement key of a member observed to no longer hold the blob, but
// keeps the key of a member still holding (the digest stays scannable until the
// bytes are confirmed gone everywhere).
func TestReconcileOrphanedPrunesObservedAbsentMember(t *testing.T) {
	n1, n2 := uuid.New(), uuid.New()
	digest := "d0"
	f := &reconcileStoreFake{
		durabilityStoreFake: durabilityStoreFake{
			refs:    map[string][]uuid.UUID{digest: {}},
			holders: map[string][]uuid.UUID{digest: {n1}},
			nodes: []store.Node{
				{ID: n1, Name: "node-1", Status: store.NodeStatusReady},
				{ID: n2, Name: "node-2", Status: store.NodeStatusReady},
			},
		},
		placement: map[string]map[uuid.UUID]bool{digest: {n1: true, n2: true}},
	}
	if err := ReconcileFunc(f, 5*time.Minute, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.removed) != 1 || f.removed[0].node != n2 {
		t.Errorf("removed = %+v, want one prune of the observed-absent member n2", f.removed)
	}
}

// TestReconcileOverReplicationReclaimsToK proves that a referenced digest with
// more live holders than K reclaims the surplus down to K, keeping the rendezvous
// top-K holders, and never enqueues a replicate.
func TestReconcileOverReplicationReclaimsToK(t *testing.T) {
	n1, n2, n3 := uuid.New(), uuid.New(), uuid.New()
	pool := "gold"
	digest := "d0"
	snapID := uuid.New()
	holders := []uuid.UUID{n1, n2, n3}
	f := &reconcileStoreFake{
		durabilityStoreFake: durabilityStoreFake{
			refs:    map[string][]uuid.UUID{digest: {snapID}},
			snaps:   map[uuid.UUID]store.Snapshot{snapID: {ID: snapID, ArtifactPoolName: &pool}},
			pools:   map[string]store.ArtifactPool{pool: {Name: pool, ReplicationFactor: store.ReplicationFactor{Count: 2}, Membership: store.ArtifactPoolMembership{AllNodes: true}}},
			holders: map[string][]uuid.UUID{digest: holders},
			nodes: []store.Node{
				{ID: n1, Name: "node-1", Status: store.NodeStatusReady},
				{ID: n2, Name: "node-2", Status: store.NodeStatusReady},
				{ID: n3, Name: "node-3", Status: store.NodeStatusReady},
			},
		},
		placement: map[string]map[uuid.UUID]bool{digest: {n1: true, n2: true, n3: true}},
	}
	if err := ReconcileFunc(f, 5*time.Minute, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.reclaimed) != 1 {
		t.Fatalf("reclaimed %d holders, want exactly 1 (3 holders down to K=2)", len(f.reclaimed))
	}
	if len(f.enqueued) != 0 {
		t.Errorf("enqueued %d replicate tasks while over-replicated, want 0", len(f.enqueued))
	}
	keepers := map[uuid.UUID]bool{}
	for _, k := range selectTargets(digest, holders, 2) {
		keepers[k] = true
	}
	if keepers[f.reclaimed[0].TargetNodeID] {
		t.Errorf("reclaimed a keeper %s, want only the rendezvous surplus holder", f.reclaimed[0].TargetNodeID)
	}
}

// TestReconcileOverReplicationReclaimsHRWLowest proves the reclaim victim is the
// rendezvous-lowest holder, never a keeper: with 3 live holders and K=2 the single
// reclaimed node is exactly the one holder not in selectTargets(digest, holders, 2),
// so the K rendezvous-top keepers are never the victim regardless of which holder
// happens to weigh high.
func TestReconcileOverReplicationReclaimsHRWLowest(t *testing.T) {
	n1, n2, n3 := uuid.New(), uuid.New(), uuid.New()
	pool := "gold"
	digest := "d0"
	snapID := uuid.New()
	holders := []uuid.UUID{n1, n2, n3}
	f := &reconcileStoreFake{
		durabilityStoreFake: durabilityStoreFake{
			refs:    map[string][]uuid.UUID{digest: {snapID}},
			snaps:   map[uuid.UUID]store.Snapshot{snapID: {ID: snapID, ArtifactPoolName: &pool}},
			pools:   map[string]store.ArtifactPool{pool: {Name: pool, ReplicationFactor: store.ReplicationFactor{Count: 2}, Membership: store.ArtifactPoolMembership{AllNodes: true}}},
			holders: map[string][]uuid.UUID{digest: holders},
			nodes: []store.Node{
				{ID: n1, Name: "node-1", Status: store.NodeStatusReady},
				{ID: n2, Name: "node-2", Status: store.NodeStatusReady},
				{ID: n3, Name: "node-3", Status: store.NodeStatusReady},
			},
		},
		placement: map[string]map[uuid.UUID]bool{digest: {n1: true, n2: true, n3: true}},
	}
	if err := ReconcileFunc(f, 5*time.Minute, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.reclaimed) != 1 {
		t.Fatalf("reclaimed %d holders, want exactly 1 (3 holders down to K=2)", len(f.reclaimed))
	}

	keepers := map[uuid.UUID]bool{}
	for _, k := range selectTargets(digest, holders, 2) {
		keepers[k] = true
	}
	var wantVictim uuid.UUID
	for _, h := range holders {
		if !keepers[h] {
			wantVictim = h
		}
	}
	if got := f.reclaimed[0].TargetNodeID; got != wantVictim {
		t.Errorf("reclaimed %s, want the lone non-keeper holder %s", got, wantVictim)
	}
}

// TestReconcileReferencedBelowKUnchanged proves the add-to-K replicate path is
// untouched by the GC branches: a referenced digest below K still adds a target
// and enqueues exactly one replicate, and reclaims nothing.
func TestReconcileReferencedBelowKUnchanged(t *testing.T) {
	n1, n2 := uuid.New(), uuid.New()
	pool := "gold"
	digest := "d0"
	snapID := uuid.New()
	f := &reconcileStoreFake{
		durabilityStoreFake: durabilityStoreFake{
			refs:    map[string][]uuid.UUID{digest: {snapID}},
			snaps:   map[uuid.UUID]store.Snapshot{snapID: {ID: snapID, ArtifactPoolName: &pool}},
			pools:   map[string]store.ArtifactPool{pool: {Name: pool, ReplicationFactor: store.ReplicationFactor{Count: 2}, Membership: store.ArtifactPoolMembership{AllNodes: true}}},
			holders: map[string][]uuid.UUID{digest: {n1}},
			nodes: []store.Node{
				{ID: n1, Name: "node-1", Status: store.NodeStatusReady},
				{ID: n2, Name: "node-2", Status: store.NodeStatusReady},
			},
		},
		placement: map[string]map[uuid.UUID]bool{digest: {n1: true}},
	}
	if err := ReconcileFunc(f, 5*time.Minute, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.reclaimed) != 0 {
		t.Errorf("reclaimed %d while below K, want 0", len(f.reclaimed))
	}
	if len(f.enqueued) != 1 {
		t.Errorf("enqueued %d replicate tasks, want 1 (add-to-K path)", len(f.enqueued))
	}
}

// TestReconcileBackstopReclaimsObservedOrphan proves the union of observed
// node_blobs digests is reconciled: a blob a node still holds but whose placement
// key was already pruned (so AllPlacementDigests omits it) is collected through the
// reclaim path, closing the residual leak. The digest reaches reconcileDigest only
// via the node_blobs union, never via the placement scan.
func TestReconcileBackstopReclaimsObservedOrphan(t *testing.T) {
	n1 := uuid.New()
	digest := "0bada550000000000000000000000000000000000000000000000000000000aa"
	f := &reconcileStoreFake{
		durabilityStoreFake: durabilityStoreFake{
			// Zero referencing snapshots -> orphaned; the node still holds the blob.
			refs:    map[string][]uuid.UUID{digest: nil},
			holders: map[string][]uuid.UUID{digest: {n1}},
			nodes:   []store.Node{{ID: n1, Name: "node-1", Status: store.NodeStatusReady}},
		},
		// No placement entry: AllPlacementDigests must not return this digest.
		placement:       map[string]map[uuid.UUID]bool{},
		nodeBlobDigests: []string{digest},
	}

	if err := ReconcileFunc(f, 5*time.Minute, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(f.reclaimed); got != 1 {
		t.Errorf("backstop reclaim enqueues = %d, want 1 (observed orphan, no placement key)", got)
	}
}

// TestReconcileBackstopSkipsReferencedObservedBlob proves the widened iteration
// does not over-collect: an observed blob that is still referenced by a snapshot is
// not orphaned and is never reclaimed.
func TestReconcileBackstopSkipsReferencedObservedBlob(t *testing.T) {
	n1 := uuid.New()
	digest := "0bada550000000000000000000000000000000000000000000000000000000bb"
	pool := "gold"
	snapID := uuid.New()
	f := &reconcileStoreFake{
		durabilityStoreFake: durabilityStoreFake{
			refs:    map[string][]uuid.UUID{digest: {snapID}},
			snaps:   map[uuid.UUID]store.Snapshot{snapID: {ID: snapID, ArtifactPoolName: &pool}},
			pools:   map[string]store.ArtifactPool{pool: {Name: pool, ReplicationFactor: store.ReplicationFactor{Count: 1}, Membership: store.ArtifactPoolMembership{AllNodes: true}}},
			holders: map[string][]uuid.UUID{digest: {n1}},
			nodes:   []store.Node{{ID: n1, Name: "node-1", Status: store.NodeStatusReady}},
		},
		placement:       map[string]map[uuid.UUID]bool{},
		nodeBlobDigests: []string{digest},
	}

	if err := ReconcileFunc(f, 5*time.Minute, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(f.reclaimed); got != 0 {
		t.Errorf("reclaim enqueues = %d, want 0 (a referenced observed blob is never collected)", got)
	}
}
