// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package replication

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// durabilityStoreFake is an in-memory DurabilityStore.
type durabilityStoreFake struct {
	refs    map[string][]uuid.UUID // digest -> snapshot ids
	snaps   map[uuid.UUID]store.Snapshot
	pools   map[string]store.ArtifactPool
	holders map[string][]uuid.UUID // digest -> node ids reporting it
	nodes   []store.Node
}

func (f *durabilityStoreFake) SnapshotsReferencingBlob(_ context.Context, d string) ([]uuid.UUID, error) {
	return f.refs[d], nil
}

func (f *durabilityStoreFake) SnapshotByID(_ context.Context, id uuid.UUID) (store.Snapshot, error) {
	s, ok := f.snaps[id]
	if !ok {
		return store.Snapshot{}, store.ErrNotFound
	}
	return s, nil
}

func (f *durabilityStoreFake) ArtifactPoolByName(_ context.Context, n string) (store.ArtifactPool, error) {
	p, ok := f.pools[n]
	if !ok {
		return store.ArtifactPool{}, store.ErrNotFound
	}
	return p, nil
}

func (f *durabilityStoreFake) BlobHolders(_ context.Context, d string) ([]uuid.UUID, error) {
	return f.holders[d], nil
}
func (f *durabilityStoreFake) AllNodes(_ context.Context) ([]store.Node, error) { return f.nodes, nil }

func TestSnapshotDurability(t *testing.T) {
	n1, n2, n3 := uuid.New(), uuid.New(), uuid.New()
	pool := "gold"
	digest := "d0"
	snapID := uuid.New()

	base := func() *durabilityStoreFake {
		return &durabilityStoreFake{
			refs:  map[string][]uuid.UUID{digest: {snapID}},
			snaps: map[uuid.UUID]store.Snapshot{snapID: {ID: snapID, ArtifactPoolName: &pool}},
			pools: map[string]store.ArtifactPool{pool: {Name: pool, ReplicationFactor: store.ReplicationFactor{Count: 2}, Membership: store.ArtifactPoolMembership{AllNodes: true}}},
			nodes: []store.Node{
				{ID: n1, Name: "node-1", Status: store.NodeStatusReady},
				{ID: n2, Name: "node-2", Status: store.NodeStatusReady},
				{ID: n3, Name: "node-3", Status: store.NodeStatusReady},
			},
		}
	}
	snap := store.Snapshot{ID: snapID, ArtifactPoolName: &pool, Disks: []store.SnapshotDisk{{SHA256: digest}}}

	// 2 holders, K=2 -> durable; holder names are the two live holders, sorted.
	f := base()
	f.holders = map[string][]uuid.UUID{digest: {n1, n2}}
	got, des, obs, holders, _ := SnapshotDurability(context.Background(), f, snap)
	if got != DurabilityDurable || des != 2 || obs != 2 {
		t.Errorf("2 holders: got %s des=%d obs=%d, want durable 2 2", got, des, obs)
	}
	if diff := cmp.Diff([]string{"node-1", "node-2"}, holders); diff != "" {
		t.Errorf("2 holders names mismatch (-want +got):\n%s", diff)
	}

	// 1 holder, K=2, a free eligible node -> replicating.
	f = base()
	f.holders = map[string][]uuid.UUID{digest: {n1}}
	if got, _, obs, _, _ := SnapshotDurability(context.Background(), f, snap); got != DurabilityReplicating || obs != 1 {
		t.Errorf("1 holder + room: got %s obs=%d, want replicating 1", got, obs)
	}

	// 0 live holders -> degraded.
	f = base()
	f.holders = map[string][]uuid.UUID{digest: {}}
	if got, _, obs, _, _ := SnapshotDurability(context.Background(), f, snap); got != DurabilityDegraded || obs != 0 {
		t.Errorf("0 holders: got %s obs=%d, want degraded 0", got, obs)
	}

	// holder node is unreachable -> not counted.
	f = base()
	f.nodes[0].Status = store.NodeStatusUnreachable
	f.holders = map[string][]uuid.UUID{digest: {n1}}
	if got, _, obs, _, _ := SnapshotDurability(context.Background(), f, snap); obs != 0 {
		t.Errorf("unreachable holder observed = %d, want 0", obs)
	} else if got == DurabilityDurable {
		t.Errorf("unreachable holder: got durable, want not durable")
	}

	// no disks yet -> unknown.
	if got, _, _, _, _ := SnapshotDurability(context.Background(), base(), store.Snapshot{ID: snapID}); got != DurabilityUnknown {
		t.Errorf("no disks: got %s, want unknown", got)
	}
}

func TestDesiredKAllSentinelAndUnresolved(t *testing.T) {
	n1, n2 := uuid.New(), uuid.New()
	nodes := []store.Node{
		{ID: n1, Name: "node-1", Status: store.NodeStatusReady},
		{ID: n2, Name: "node-2", Status: store.NodeStatusReady},
	}
	pool := "everywhere"
	snapID := uuid.New()
	f := &durabilityStoreFake{
		refs:  map[string][]uuid.UUID{"d": {snapID}},
		snaps: map[uuid.UUID]store.Snapshot{snapID: {ID: snapID, ArtifactPoolName: &pool}},
		pools: map[string]store.ArtifactPool{pool: {Name: pool, ReplicationFactor: store.ReplicationFactor{All: true}, Membership: store.ArtifactPoolMembership{AllNodes: true}}},
		nodes: nodes,
	}
	k, _, err := blobPlacementTarget(context.Background(), f, "d", nodes, liveNodeIDs(nodes))
	if err != nil || k != 2 {
		t.Errorf("all-sentinel K = %d, %v; want 2 (live member count)", k, err)
	}

	f.snaps[snapID] = store.Snapshot{ID: snapID, ArtifactPoolName: nil}
	k, _, _ = blobPlacementTarget(context.Background(), f, "d", nodes, liveNodeIDs(nodes))
	if k != 1 {
		t.Errorf("unresolved pool K = %d, want 1", k)
	}
}
