// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package gateways

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func vni(v int32) *int32 { return &v }

// reconcileStoreFake is a seam double for GatewayReconcileStore. It records every
// CreateGatewayMembership call and never offers a delete, so a removal attempt
// would not compile - the never-remove invariant is enforced structurally.
type reconcileStoreFake struct {
	networks      []store.Network
	nicsByNetwork map[uuid.UUID][]store.VMNic
	nodes         []store.Node
	memberships   map[uuid.UUID][]store.GatewayMembership // networkID -> members
	created       []struct{ gateway, network uuid.UUID }
}

func (f *reconcileStoreFake) ListNetworks(_ context.Context, arg store.ListNetworksParams) ([]store.Network, error) {
	out := make([]store.Network, 0, len(f.networks))
	for _, n := range f.networks {
		if arg.Type != nil && n.Type != *arg.Type {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

func (f *reconcileStoreFake) ListVMNicsByNetwork(_ context.Context, networkID uuid.UUID) ([]store.VMNic, error) {
	return f.nicsByNetwork[networkID], nil
}

func (f *reconcileStoreFake) AllNodes(context.Context) ([]store.Node, error) { return f.nodes, nil }

func (f *reconcileStoreFake) ListGatewayMembershipsForNetwork(_ context.Context, networkID uuid.UUID) ([]store.GatewayMembership, error) {
	return f.memberships[networkID], nil
}

func (f *reconcileStoreFake) CreateGatewayMembership(_ context.Context, gatewayID, networkID uuid.UUID) (store.GatewayMembership, error) {
	for _, m := range f.memberships[networkID] {
		if m.GatewayID == gatewayID {
			return store.GatewayMembership{}, store.ErrGatewayMembershipExists
		}
	}
	m := store.GatewayMembership{GatewayID: gatewayID, NetworkID: networkID}
	if f.memberships == nil {
		f.memberships = map[uuid.UUID][]store.GatewayMembership{}
	}
	f.memberships[networkID] = append(f.memberships[networkID], m)
	f.created = append(f.created, struct{ gateway, network uuid.UUID }{gatewayID, networkID})
	return m, nil
}

func gatewayNode(id uuid.UUID) store.Node {
	return store.Node{ID: id, Kind: store.NodeKindGateway, Status: store.NodeStatusReady}
}

func TestReconcileEnsuresTwoGatewaysCoverIngressNetwork(t *testing.T) {
	netID := uuid.New()
	g1, g2, g3 := uuid.New(), uuid.New(), uuid.New()
	f := &reconcileStoreFake{
		networks: []store.Network{{ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		nicsByNetwork: map[uuid.UUID][]store.VMNic{
			netID: {{ID: uuid.New(), NetworkID: netID}},
		},
		nodes:       []store.Node{gatewayNode(g1), gatewayNode(g2), gatewayNode(g3)},
		memberships: map[uuid.UUID][]store.GatewayMembership{},
	}

	if err := ReconcileFunc(f, ReconcileConfig{}, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(f.memberships[netID]); got < 2 {
		t.Fatalf("coverage = %d memberships, want >= 2", got)
	}
}

func TestReconcileBestEffortWithSingleGateway(t *testing.T) {
	netID := uuid.New()
	g1 := uuid.New()
	f := &reconcileStoreFake{
		networks: []store.Network{{ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		nicsByNetwork: map[uuid.UUID][]store.VMNic{
			netID: {{ID: uuid.New(), NetworkID: netID}},
		},
		nodes:       []store.Node{gatewayNode(g1)},
		memberships: map[uuid.UUID][]store.GatewayMembership{},
	}

	if err := ReconcileFunc(f, ReconcileConfig{}, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile returned error, want best-effort nil: %v", err)
	}
	if got := len(f.created); got != 1 {
		t.Fatalf("created %d memberships, want 1 (only one live gateway)", got)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	netID := uuid.New()
	g1, g2 := uuid.New(), uuid.New()
	f := &reconcileStoreFake{
		networks: []store.Network{{ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		nicsByNetwork: map[uuid.UUID][]store.VMNic{
			netID: {{ID: uuid.New(), NetworkID: netID}},
		},
		nodes:       []store.Node{gatewayNode(g1), gatewayNode(g2)},
		memberships: map[uuid.UUID][]store.GatewayMembership{},
	}

	tick := ReconcileFunc(f, ReconcileConfig{}, discardLog())
	if err := tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	firstCreated := len(f.created)
	if err := tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if len(f.created) != firstCreated {
		t.Fatalf("second tick created %d more memberships, want 0 (idempotent)", len(f.created)-firstCreated)
	}
	if got := len(f.memberships[netID]); got != 2 {
		t.Fatalf("coverage = %d memberships after two ticks, want exactly 2 (no duplicates)", got)
	}
}

func TestReconcileSkipsNetworkWithNoVMs(t *testing.T) {
	netID := uuid.New()
	g1, g2 := uuid.New(), uuid.New()
	f := &reconcileStoreFake{
		networks:      []store.Network{{ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		nicsByNetwork: map[uuid.UUID][]store.VMNic{}, // no NICs => not ingress-eligible
		nodes:         []store.Node{gatewayNode(g1), gatewayNode(g2)},
		memberships:   map[uuid.UUID][]store.GatewayMembership{},
	}

	if err := ReconcileFunc(f, ReconcileConfig{}, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(f.created); got != 0 {
		t.Fatalf("created %d memberships for an empty overlay, want 0", got)
	}
}

func TestReconcileLeavesExistingCoverageUntouched(t *testing.T) {
	netID := uuid.New()
	g1, g2, g3 := uuid.New(), uuid.New(), uuid.New()
	existing := []store.GatewayMembership{
		{GatewayID: g1, NetworkID: netID},
		{GatewayID: g2, NetworkID: netID},
	}
	f := &reconcileStoreFake{
		networks: []store.Network{{ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		nicsByNetwork: map[uuid.UUID][]store.VMNic{
			netID: {{ID: uuid.New(), NetworkID: netID}},
		},
		nodes:       []store.Node{gatewayNode(g1), gatewayNode(g2), gatewayNode(g3)},
		memberships: map[uuid.UUID][]store.GatewayMembership{netID: existing},
	}

	if err := ReconcileFunc(f, ReconcileConfig{}, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(f.created); got != 0 {
		t.Fatalf("created %d memberships when coverage already >= 2, want 0", got)
	}
	if got := len(f.memberships[netID]); got != 2 {
		t.Fatalf("coverage = %d memberships, want the original 2 (never removed)", got)
	}
}

func TestReconcileIgnoresBridgeNetworks(t *testing.T) {
	overlayID, bridgeID := uuid.New(), uuid.New()
	g1, g2 := uuid.New(), uuid.New()
	f := &reconcileStoreFake{
		networks: []store.Network{
			{ID: overlayID, Type: store.NetworkTypeOverlay, VNI: vni(100)},
			{ID: bridgeID, Type: store.NetworkTypeBridge},
		},
		nicsByNetwork: map[uuid.UUID][]store.VMNic{
			overlayID: {{ID: uuid.New(), NetworkID: overlayID}},
			bridgeID:  {{ID: uuid.New(), NetworkID: bridgeID}},
		},
		nodes:       []store.Node{gatewayNode(g1), gatewayNode(g2)},
		memberships: map[uuid.UUID][]store.GatewayMembership{},
	}

	if err := ReconcileFunc(f, ReconcileConfig{}, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, c := range f.created {
		if c.network == bridgeID {
			t.Fatalf("created a gateway membership on a bridge network %s", bridgeID)
		}
	}
}

// errCreateStoreFake fails CreateGatewayMembership to prove the tick is fail-open.
type errCreateStoreFake struct {
	reconcileStoreFake
}

func (f *errCreateStoreFake) CreateGatewayMembership(context.Context, uuid.UUID, uuid.UUID) (store.GatewayMembership, error) {
	return store.GatewayMembership{}, errors.New("transient etcd error")
}

func TestReconcileFailOpenOnCreateError(t *testing.T) {
	netID := uuid.New()
	g1, g2 := uuid.New(), uuid.New()
	f := &errCreateStoreFake{reconcileStoreFake{
		networks: []store.Network{{ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		nicsByNetwork: map[uuid.UUID][]store.VMNic{
			netID: {{ID: uuid.New(), NetworkID: netID}},
		},
		nodes:       []store.Node{gatewayNode(g1), gatewayNode(g2)},
		memberships: map[uuid.UUID][]store.GatewayMembership{},
	}}

	if err := ReconcileFunc(f, ReconcileConfig{}, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile must fail open on a create error, got %v", err)
	}
}
