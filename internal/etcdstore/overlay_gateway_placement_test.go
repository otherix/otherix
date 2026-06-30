// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/internal/store"
)

// TestOverlayPlacementsIncludeGatewayMembership pins the gateway FDB seam: a
// gateway covers an overlay network without hosting a VM, so its membership must
// surface in the same overlay placement projection every NIC placement does -
// carrying the gateway's MAC and its node id - so peers learn the gateway VTEP
// and the gateway becomes a post-cutover nudge target. The projection therefore
// returns both the VM NIC placement and the synthetic gateway placement, and the
// gateway node appears as a peer of a VM sharing its VNI.
func TestOverlayPlacementsIncludeGatewayMembership(t *testing.T) {
	ctx := context.Background()
	s, cli := startStore(t)

	n1 := seedNodeWithEndpoint(t, s, "vm-host", "https://node-1:9443")
	gw1 := seedNodeWithEndpoint(t, s, "gateway", "https://gw-1:9443")

	ov := overlayNet(t, s, "10.70.0.0/24")

	vmID, vmNicID := seedVMWithOverlayNICIndexed(t, cli, ov.ID, "52:54:00:00:07:0a")
	placeVM(t, cli, vmID, n1.ID)
	_ = vmNicID

	m, err := s.CreateGatewayMembership(ctx, gw1.ID, ov.ID)
	if err != nil {
		t.Fatalf("CreateGatewayMembership: %v", err)
	}

	got, _, err := s.ListOverlayNICPlacementsPinned(ctx)
	if err != nil {
		t.Fatalf("ListOverlayNICPlacementsPinned: %v", err)
	}

	want := []store.OverlayNICPlacement{
		{VNI: *ov.VNI, Mac: mustMAC(t, "52:54:00:00:07:0a"), NodeID: n1.ID},
		{VNI: *ov.VNI, Mac: m.MAC, NodeID: gw1.ID},
	}
	less := func(a, b store.OverlayNICPlacement) bool { return a.Mac.String() < b.Mac.String() }
	sort.Slice(got, func(i, j int) bool { return less(got[i], got[j]) })
	sort.Slice(want, func(i, j int) bool { return less(want[i], want[j]) })
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("placements mismatch (-want +got):\n%s", diff)
	}

	peers, err := s.OverlayPeerNodesForVM(ctx, vmID)
	if err != nil {
		t.Fatalf("OverlayPeerNodesForVM: %v", err)
	}
	hasGW := false
	for _, p := range peers {
		if p.ID == gw1.ID {
			hasGW = true
		}
	}
	if !hasGW {
		t.Errorf("gateway node %s missing from VM peer set %v", gw1.ID, peers)
	}
}
