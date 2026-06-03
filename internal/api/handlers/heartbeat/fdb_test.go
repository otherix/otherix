// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

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
	h := &Handler{log: discardLogger()} // loadDeclaredFDB now logs; provide a logger
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

func TestLoadDeclaredFDBSkipsGoneNode(t *testing.T) {
	nodeA, nodeB := uuid.New(), uuid.New()
	macA, _ := net.ParseMAC("52:54:00:00:00:0a")
	macB, _ := net.ParseMAC("52:54:00:00:00:0b")
	hp := &fakeFDBProjection{
		placements: []store.OverlayNICPlacement{
			{VNI: 1000, Mac: macA, NodeID: nodeA},
			{VNI: 1000, Mac: macB, NodeID: nodeB},
		},
		wg: []store.AgentWireguard{
			{NodeID: nodeA, OverlayIP: netip.MustParseAddr("10.42.0.1")},
			{NodeID: nodeB, OverlayIP: netip.MustParseAddr("10.42.0.2")},
		},
		gone: map[uuid.UUID]bool{nodeB: true},
	}
	h := &Handler{log: discardLogger()}
	got, err := h.loadDeclaredFDB(context.Background(), hp, nodeA)
	if err != nil {
		t.Fatalf("loadDeclaredFDB: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want no entries (gone node pruned)", got)
	}
}

func TestLoadDeclaredFDBSkipsNotFoundNode(t *testing.T) {
	nodeA, nodeB := uuid.New(), uuid.New()
	macA, _ := net.ParseMAC("52:54:00:00:00:0a")
	macB, _ := net.ParseMAC("52:54:00:00:00:0b")
	hp := &fakeFDBProjection{
		placements: []store.OverlayNICPlacement{
			{VNI: 1000, Mac: macA, NodeID: nodeA},
			{VNI: 1000, Mac: macB, NodeID: nodeB},
		},
		wg: []store.AgentWireguard{
			{NodeID: nodeA, OverlayIP: netip.MustParseAddr("10.42.0.1")},
			{NodeID: nodeB, OverlayIP: netip.MustParseAddr("10.42.0.2")},
		},
		notFound: map[uuid.UUID]bool{nodeB: true},
	}
	h := &Handler{log: discardLogger()}
	got, err := h.loadDeclaredFDB(context.Background(), hp, nodeA)
	if err != nil {
		t.Fatalf("loadDeclaredFDB: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want no entries (soft-deleted node pruned)", got)
	}
}

type fakeFDBProjection struct {
	store.HeartbeatProjection
	placements []store.OverlayNICPlacement
	wg         []store.AgentWireguard
	gone       map[uuid.UUID]bool
	notFound   map[uuid.UUID]bool
}

func (f *fakeFDBProjection) ListOverlayNICPlacements(context.Context) ([]store.OverlayNICPlacement, error) {
	return f.placements, nil
}

// ListOverlayNICPlacementsPinned reports a fixed snapshot revision (1) so the
// projection threads a non-zero rev through the rest of the join; the fake's
// reads ignore it (in-memory data has no MVCC dimension), but the wiring is
// exercised - the projection must call the pinned variants.
func (f *fakeFDBProjection) ListOverlayNICPlacementsPinned(context.Context) ([]store.OverlayNICPlacement, int64, error) {
	return f.placements, 1, nil
}

func (f *fakeFDBProjection) ListAgentWireguard(context.Context) ([]store.AgentWireguard, error) {
	return f.wg, nil
}

func (f *fakeFDBProjection) ListAgentWireguardAtRev(_ context.Context, _ int64) ([]store.AgentWireguard, error) {
	return f.wg, nil
}

func (f *fakeFDBProjection) NodeByID(_ context.Context, id uuid.UUID) (store.Node, error) {
	if f.notFound[id] {
		return store.Node{}, store.ErrNotFound
	}
	st := store.NodeStatusReady
	if f.gone[id] {
		st = store.NodeStatusGone
	}
	return store.Node{ID: id, Status: st}, nil
}

func (f *fakeFDBProjection) NodeByIDAtRev(ctx context.Context, id uuid.UUID, _ int64) (store.Node, error) {
	return f.NodeByID(ctx, id)
}
