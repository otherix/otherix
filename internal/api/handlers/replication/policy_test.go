// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package replication

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestSelectTargetsDeterministicAndBounded(t *testing.T) {
	a := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	b := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	c := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	eligible := []uuid.UUID{a, b, c}

	got1 := selectTargets("deadbeef", eligible, 2)
	got2 := selectTargets("deadbeef", []uuid.UUID{c, a, b}, 2) // input order must not matter
	if len(got1) != 2 {
		t.Fatalf("selectTargets need=2 returned %d", len(got1))
	}
	if got1[0] != got2[0] || got1[1] != got2[1] {
		t.Errorf("selectTargets not deterministic across input order: %v vs %v", got1, got2)
	}
	if n := selectTargets("deadbeef", eligible, 5); len(n) != 3 {
		t.Errorf("selectTargets need>len returned %d, want 3 (capped)", len(n))
	}
	if n := selectTargets("deadbeef", eligible, 0); len(n) != 0 {
		t.Errorf("selectTargets need=0 returned %d, want 0", len(n))
	}
}

func TestMembershipNodeIDsAllNodesSkipsDead(t *testing.T) {
	live := store.Node{ID: uuid.New(), Name: "node-1", Status: store.NodeStatusReady}
	dead := store.Node{ID: uuid.New(), Name: "node-2", Status: store.NodeStatusGone}
	nodes := []store.Node{live, dead}

	got := membershipNodeIDs(store.ArtifactPoolMembership{AllNodes: true}, nodes)
	if len(got) != 1 || !got[live.ID] {
		t.Errorf("membershipNodeIDs AllNodes = %v, want only live node", got)
	}

	byName := membershipNodeIDs(store.ArtifactPoolMembership{Nodes: []string{"NODE-1", "node-2"}}, nodes)
	if len(byName) != 1 || !byName[live.ID] {
		t.Errorf("membershipNodeIDs by name = %v, want only live node-1 (case-insensitive)", byName)
	}
}

func TestLiveNodeIDsExcludesDeadAndDeleted(t *testing.T) {
	now := time.Now()
	ready := store.Node{ID: uuid.New(), Status: store.NodeStatusReady}
	unreachable := store.Node{ID: uuid.New(), Status: store.NodeStatusUnreachable}
	gone := store.Node{ID: uuid.New(), Status: store.NodeStatusGone}
	deleted := store.Node{ID: uuid.New(), Status: store.NodeStatusReady, DeletedAt: &now}
	nodes := []store.Node{ready, unreachable, gone, deleted}

	live := liveNodeIDs(nodes)
	if len(live) != 1 || !live[ready.ID] {
		t.Errorf("liveNodeIDs = %v, want only ready node", live)
	}
}

func TestRebalanceEligibleNodeIDs(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	grace := 5 * time.Minute
	stale := now.Add(-10 * time.Minute)
	fresh := now.Add(-1 * time.Minute)
	created := now.Add(-10 * time.Minute)
	unreachableStale := store.Node{ID: uuid.New(), Status: store.NodeStatusUnreachable, LastHeartbeatAt: &stale}
	unreachableFresh := store.Node{ID: uuid.New(), Status: store.NodeStatusUnreachable, LastHeartbeatAt: &fresh}
	readyStale := store.Node{ID: uuid.New(), Status: store.NodeStatusReady, LastHeartbeatAt: &stale}
	neverHeartbeated := store.Node{ID: uuid.New(), Status: store.NodeStatusUnreachable, CreatedAt: created}
	got := rebalanceEligibleNodeIDs([]store.Node{unreachableStale, unreachableFresh, readyStale, neverHeartbeated}, grace, now)
	want := map[uuid.UUID]bool{unreachableStale.ID: true, neverHeartbeated.ID: true}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("rebalanceEligibleNodeIDs mismatch (-want +got):\n%s", diff)
	}
}
