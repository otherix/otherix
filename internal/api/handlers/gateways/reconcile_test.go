// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package gateways

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func vni(v int32) *int32 { return &v }

// reconcileStoreFake is a seam double for GatewayReconcileStore. It records every
// CreateGatewayMembership and DeleteGatewayMembership call so a test can assert
// both the additive coverage pass and the sticky reaping pass act exactly when
// they should.
type reconcileStoreFake struct {
	networks        []store.Network
	nicsByNetwork   map[uuid.UUID][]store.VMNic
	nodes           []store.Node
	memberships     map[uuid.UUID][]store.GatewayMembership // networkID -> members
	statusByNetwork map[uuid.UUID][]store.NetworkNodeStatus // networkID -> per-node status
	created         []struct{ gateway, network uuid.UUID }
	deleted         []struct{ gateway, network uuid.UUID }
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

func (f *reconcileStoreFake) ListNetworkNodeStatusByNetwork(_ context.Context, networkID uuid.UUID) ([]store.NetworkNodeStatus, error) {
	return f.statusByNetwork[networkID], nil
}

func (f *reconcileStoreFake) DeleteGatewayMembership(_ context.Context, gatewayID, networkID uuid.UUID) error {
	members := f.memberships[networkID]
	kept := members[:0:0]
	for _, m := range members {
		if m.GatewayID == gatewayID {
			continue
		}
		kept = append(kept, m)
	}
	f.memberships[networkID] = kept
	f.deleted = append(f.deleted, struct{ gateway, network uuid.UUID }{gatewayID, networkID})
	return nil
}

func gatewayNode(id uuid.UUID) store.Node {
	return store.Node{ID: id, GatewayRole: true, Status: store.NodeStatusReady}
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

func TestReconcilePlacesEveryLiveGatewayOnActiveOverlay(t *testing.T) {
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
	members, err := f.ListGatewayMembershipsForNetwork(context.Background(), netID)
	if err != nil {
		t.Fatalf("list memberships: %v", err)
	}
	if got, want := len(members), 3; got != want {
		t.Errorf("ListGatewayMembershipsForNetwork(active overlay) = %d memberships, want %d (one per live gateway)", got, want)
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

func TestReconcileKeepsExistingAndAddsMissingGateway(t *testing.T) {
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
	// Cross-product: the two existing memberships are preserved and only the
	// missing live gateway (g3) is added, so no membership is ever recreated.
	if got := len(f.created); got != 1 {
		t.Fatalf("created %d memberships, want 1 (only the missing gateway g3)", got)
	}
	if got := len(f.created); got == 1 && f.created[0].gateway != g3 {
		t.Errorf("created membership for %s, want the missing gateway %s", f.created[0].gateway, g3)
	}
	have := map[uuid.UUID]bool{}
	for _, m := range f.memberships[netID] {
		have[m.GatewayID] = true
	}
	if !have[g1] || !have[g2] || !have[g3] {
		t.Errorf("coverage = %v, want one membership per live gateway (g1, g2, g3)", f.memberships[netID])
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

func TestReapReapsMembershipWhenRoleTurnedOff(t *testing.T) {
	netID := uuid.New()
	gw := uuid.New()
	f := &reconcileStoreFake{
		networks: []store.Network{{ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		nicsByNetwork: map[uuid.UUID][]store.VMNic{
			netID: {{ID: uuid.New(), NetworkID: netID}}, // active overlay
		},
		// A live node that no longer holds the gateway role (operator ran
		// gateway disable): still reachable, but not eligible for coverage.
		nodes:       []store.Node{{ID: gw, GatewayRole: false, Status: store.NodeStatusReady}},
		memberships: map[uuid.UUID][]store.GatewayMembership{netID: {{GatewayID: gw, NetworkID: netID}}},
		statusByNetwork: map[uuid.UUID][]store.NetworkNodeStatus{
			netID: {{NodeID: gw, ActiveSessions: 0}},
		},
	}

	if err := ReconcileFunc(f, ReconcileConfig{}, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(f.memberships[netID]); got != 0 {
		t.Errorf("membership for a role-off gateway with 0 sessions survived: %d memberships, want 0 (reaped)", got)
	}
}

func TestReapKeepsRoleOffMembershipWhileSessionsDrain(t *testing.T) {
	netID := uuid.New()
	gw := uuid.New()
	f := &reconcileStoreFake{
		networks: []store.Network{{ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		nicsByNetwork: map[uuid.UUID][]store.VMNic{
			netID: {{ID: uuid.New(), NetworkID: netID}}, // active overlay
		},
		nodes:       []store.Node{{ID: gw, GatewayRole: false, Status: store.NodeStatusReady}},
		memberships: map[uuid.UUID][]store.GatewayMembership{netID: {{GatewayID: gw, NetworkID: netID}}},
		statusByNetwork: map[uuid.UUID][]store.NetworkNodeStatus{
			netID: {{NodeID: gw, ActiveSessions: 3}}, // still draining
		},
	}

	if err := ReconcileFunc(f, ReconcileConfig{}, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := len(f.memberships[netID]); got != 1 {
		t.Errorf("role-off membership drained instead of held: %d memberships, want 1 (a live session must not be cut)", got)
	}
	if len(f.deleted) != 0 {
		t.Errorf("DeleteGatewayMembership called %d times while a session was still live, want 0", len(f.deleted))
	}
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

func TestSubnetAddressPressure(t *testing.T) {
	mustPrefix := func(s string) netip.Prefix {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			t.Fatalf("ParsePrefix(%q) = %v", s, err)
		}
		return p
	}
	tests := []struct {
		name            string
		gatewayCount    int
		vmNicCount      int
		subnet          netip.Prefix
		wantOver        bool
		wantUsableHosts int
	}{
		{
			name:            "slash24 under fraction",
			gatewayCount:    10,
			vmNicCount:      90,
			subnet:          mustPrefix("10.0.0.0/24"),
			wantOver:        false,
			wantUsableHosts: 253,
		},
		{
			name:            "slash24 over fraction",
			gatewayCount:    100,
			vmNicCount:      110,
			subnet:          mustPrefix("10.0.0.0/24"),
			wantOver:        true,
			wantUsableHosts: 253,
		},
		{
			name:            "slash24 exactly at fraction is over",
			gatewayCount:    203,
			vmNicCount:      0,
			subnet:          mustPrefix("10.0.0.0/24"),
			wantOver:        true,
			wantUsableHosts: 253,
		},
		{
			name:            "slash30 tiny subnet no panic",
			gatewayCount:    0,
			vmNicCount:      0,
			subnet:          mustPrefix("10.0.0.0/30"),
			wantOver:        false,
			wantUsableHosts: 1,
		},
		{
			name:            "slash31 no usable hosts",
			gatewayCount:    5,
			vmNicCount:      5,
			subnet:          mustPrefix("10.0.0.0/31"),
			wantOver:        false,
			wantUsableHosts: 0,
		},
		{
			name:            "slash32 no usable hosts",
			gatewayCount:    5,
			vmNicCount:      5,
			subnet:          mustPrefix("10.0.0.0/32"),
			wantOver:        false,
			wantUsableHosts: 0,
		},
		{
			name:            "invalid zero prefix",
			gatewayCount:    1000,
			vmNicCount:      1000,
			subnet:          netip.Prefix{},
			wantOver:        false,
			wantUsableHosts: 0,
		},
		{
			name:            "ipv6 prefix not supported",
			gatewayCount:    1000,
			vmNicCount:      1000,
			subnet:          mustPrefix("fd00::/64"),
			wantOver:        false,
			wantUsableHosts: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			over, usableHosts := subnetAddressPressure(tt.gatewayCount, tt.vmNicCount, tt.subnet)
			if over != tt.wantOver {
				t.Errorf("subnetAddressPressure(%d, %d, %v) over = %v, want %v",
					tt.gatewayCount, tt.vmNicCount, tt.subnet, over, tt.wantOver)
			}
			if usableHosts != tt.wantUsableHosts {
				t.Errorf("subnetAddressPressure(%d, %d, %v) usableHosts = %v, want %v",
					tt.gatewayCount, tt.vmNicCount, tt.subnet, usableHosts, tt.wantUsableHosts)
			}
		})
	}
}
