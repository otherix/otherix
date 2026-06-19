// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package replication

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// reconcileStoreFake extends the durability fake with placement + enqueue.
type reconcileStoreFake struct {
	durabilityStoreFake
	placement map[string]map[uuid.UUID]bool // digest -> member set
	enqueued  []ReplicateArgs
	added     []struct {
		digest string
		node   uuid.UUID
	}
	removed []struct {
		digest string
		node   uuid.UUID
	}
}

func (f *reconcileStoreFake) AllPlacementDigests(context.Context) ([]string, error) {
	out := make([]string, 0, len(f.placement))
	for d := range f.placement {
		out = append(out, d)
	}
	return out, nil
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
	f.enqueued = append(f.enqueued, args.(ReplicateArgs))
	return uuid.New(), nil
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

	if err := ReconcileFunc(f, discardLog())(context.Background()); err != nil {
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

func TestReconcilePrunesGoneMemberButNotLastPointer(t *testing.T) {
	gone1, holder2, n3 := uuid.New(), uuid.New(), uuid.New()
	pool := "gold"
	digest := "d0"
	snapID := uuid.New()
	mk := func(s1 store.NodeStatus, holders []uuid.UUID, members []uuid.UUID) *reconcileStoreFake {
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
					{ID: gone1, Name: "node-1", Status: s1},
					{ID: holder2, Name: "node-2", Status: store.NodeStatusReady},
					{ID: n3, Name: "node-3", Status: store.NodeStatusReady},
				},
			},
			placement: map[string]map[uuid.UUID]bool{digest: pm},
		}
	}

	f := mk(store.NodeStatusGone, []uuid.UUID{holder2}, []uuid.UUID{gone1, holder2})
	if err := ReconcileFunc(f, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.removed) != 1 || f.removed[0].node != gone1 {
		t.Errorf("removed = %+v, want one prune of the gone member", f.removed)
	}

	f = mk(store.NodeStatusGone, []uuid.UUID{}, []uuid.UUID{gone1})
	if err := ReconcileFunc(f, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.removed) != 0 {
		t.Errorf("removed = %+v, want no prune of the last pointer", f.removed)
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
	if err := ReconcileFunc(f, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.enqueued) != 0 {
		t.Errorf("enqueued %d with no live holder, want 0 (fail toward inaction)", len(f.enqueued))
	}
}
