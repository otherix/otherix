// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestLoadDeclaredFDB(t *testing.T) {
	nodeA, nodeB := uuid.New(), uuid.New()
	macB, _ := net.ParseMAC("52:54:00:00:00:0b")
	macA, _ := net.ParseMAC("52:54:00:00:00:0a")
	hp := &fakeFDBProjection{
		placements: []store.OverlayNICPlacement{
			{VNI: 1000, Mac: macA, NodeID: nodeA},
			{VNI: 1000, Mac: macB, NodeID: nodeB},
		},
		wg: []store.AgentWireguard{
			{NodeID: nodeA, OverlayIP: netip.MustParseAddr("10.42.0.1")},
			{NodeID: nodeB, OverlayIP: netip.MustParseAddr("10.42.0.2")},
		},
	}
	h := &Handler{} // loadDeclaredFDB uses only hp + selfNodeID, no Handler state
	got, err := h.loadDeclaredFDB(context.Background(), hp, nodeA)
	if err != nil {
		t.Fatalf("loadDeclaredFDB: %v", err)
	}
	// nodeA has a local VM on VNI 1000, so it gets nodeB's unicast + a flood to nodeB.
	want := []declaredFDBEntry{
		{VNI: 1000, MAC: "00:00:00:00:00:00", VtepIP: "10.42.0.2"},
		{VNI: 1000, MAC: "52:54:00:00:00:0b", VtepIP: "10.42.0.2"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("declared_fdb mismatch (-want +got):\n%s", diff)
	}
}

type fakeFDBProjection struct {
	store.HeartbeatProjection
	placements []store.OverlayNICPlacement
	wg         []store.AgentWireguard
}

func (f *fakeFDBProjection) ListOverlayNICPlacements(context.Context) ([]store.OverlayNICPlacement, error) {
	return f.placements, nil
}

func (f *fakeFDBProjection) ListAgentWireguard(context.Context) ([]store.AgentWireguard, error) {
	return f.wg, nil
}
