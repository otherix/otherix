// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package gateways

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

type selectStoreFake struct {
	nicsByVM        map[uuid.UUID][]store.VMNic
	networks        map[uuid.UUID]store.Network
	memberships     map[uuid.UUID][]store.GatewayMembership
	statusByNetwork map[uuid.UUID][]store.NetworkNodeStatus
	nodes           []store.Node
}

func (f *selectStoreFake) ListVMNicsByVM(_ context.Context, vmID uuid.UUID) ([]store.VMNic, error) {
	return f.nicsByVM[vmID], nil
}

func (f *selectStoreFake) NetworkByID(_ context.Context, id uuid.UUID) (store.Network, error) {
	n, ok := f.networks[id]
	if !ok {
		return store.Network{}, store.ErrNotFound
	}
	return n, nil
}

func (f *selectStoreFake) ListGatewayMembershipsForNetwork(_ context.Context, networkID uuid.UUID) ([]store.GatewayMembership, error) {
	return f.memberships[networkID], nil
}

func (f *selectStoreFake) ListNetworkNodeStatusByNetwork(_ context.Context, networkID uuid.UUID) ([]store.NetworkNodeStatus, error) {
	return f.statusByNetwork[networkID], nil
}

func (f *selectStoreFake) AllNodes(context.Context) ([]store.Node, error) { return f.nodes, nil }

// TestSelectReturnsConvergedGateway proves the convergence gate: two gateways are
// members of the VM's overlay network but only one reports the network ready, so
// only the ready gateway is eligible - selecting the not-yet-converged member
// would blackhole the forward.
func TestSelectReturnsConvergedGateway(t *testing.T) {
	netID := uuid.New()
	vmID := uuid.New()
	ready, pending := uuid.New(), uuid.New()
	f := &selectStoreFake{
		nicsByVM: map[uuid.UUID][]store.VMNic{vmID: {{ID: uuid.New(), NetworkID: netID}}},
		networks: map[uuid.UUID]store.Network{netID: {ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		memberships: map[uuid.UUID][]store.GatewayMembership{netID: {
			{GatewayID: ready, NetworkID: netID},
			{GatewayID: pending, NetworkID: netID},
		}},
		statusByNetwork: map[uuid.UUID][]store.NetworkNodeStatus{netID: {
			{NetworkID: netID, NodeID: ready, ReconciliationStatus: "ready"},
			{NetworkID: netID, NodeID: pending, ReconciliationStatus: "pending"},
		}},
		nodes: []store.Node{
			{ID: ready, GatewayRole: true, Status: store.NodeStatusReady, IngressAdvertisedEndpoint: "https://gw.test:9444"},
			{ID: pending, GatewayRole: true, Status: store.NodeStatusReady, IngressAdvertisedEndpoint: "https://gw.test:9444"},
		},
	}

	got, err := SelectGatewayForVM(context.Background(), f, vmID)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if got.ID != ready {
		t.Fatalf("selected gateway %s, want the converged gateway %s", got.ID, ready)
	}
}

func TestSelectNoneReadyIsUnavailable(t *testing.T) {
	netID := uuid.New()
	vmID := uuid.New()
	g1, g2 := uuid.New(), uuid.New()
	f := &selectStoreFake{
		nicsByVM: map[uuid.UUID][]store.VMNic{vmID: {{ID: uuid.New(), NetworkID: netID}}},
		networks: map[uuid.UUID]store.Network{netID: {ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		memberships: map[uuid.UUID][]store.GatewayMembership{netID: {
			{GatewayID: g1, NetworkID: netID},
			{GatewayID: g2, NetworkID: netID},
		}},
		statusByNetwork: map[uuid.UUID][]store.NetworkNodeStatus{netID: {
			{NetworkID: netID, NodeID: g1, ReconciliationStatus: "pending"},
			{NetworkID: netID, NodeID: g2, ReconciliationStatus: "failed"},
		}},
		nodes: []store.Node{
			{ID: g1, GatewayRole: true, Status: store.NodeStatusReady},
			{ID: g2, GatewayRole: true, Status: store.NodeStatusReady},
		},
	}

	_, err := SelectGatewayForVM(context.Background(), f, vmID)
	if !errors.Is(err, ErrIngressUnavailable) {
		t.Fatalf("err = %v, want ErrIngressUnavailable", err)
	}
}

func TestSelectNoOverlayNICIsUnavailable(t *testing.T) {
	vmID := uuid.New()
	bridgeID := uuid.New()
	f := &selectStoreFake{
		nicsByVM: map[uuid.UUID][]store.VMNic{vmID: {{ID: uuid.New(), NetworkID: bridgeID}}},
		networks: map[uuid.UUID]store.Network{bridgeID: {ID: bridgeID, Type: store.NetworkTypeBridge}},
	}

	_, err := SelectGatewayForVM(context.Background(), f, vmID)
	if !errors.Is(err, ErrIngressUnavailable) {
		t.Fatalf("err = %v, want ErrIngressUnavailable for a VM with no overlay NIC", err)
	}
}

// TestSelectSkipsGatewayWithoutIngressEndpoint proves the ingress-endpoint gate:
// two gateways are live and converged members of the VM's overlay, but only one
// advertises an ingress endpoint. The endpoint-less gateway sorts lower by id yet
// must be skipped, because dialing its mTLS control endpoint would fail the TLS
// handshake - selection must return the gateway that advertises an ingress
// endpoint.
func TestSelectSkipsGatewayWithoutIngressEndpoint(t *testing.T) {
	netID := uuid.New()
	vmID := uuid.New()
	// Fixed ids so the endpoint-less gateway sorts strictly lower by string.
	noEndpoint := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	withEndpoint := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	f := &selectStoreFake{
		nicsByVM: map[uuid.UUID][]store.VMNic{vmID: {{ID: uuid.New(), NetworkID: netID}}},
		networks: map[uuid.UUID]store.Network{netID: {ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		memberships: map[uuid.UUID][]store.GatewayMembership{netID: {
			{GatewayID: noEndpoint, NetworkID: netID},
			{GatewayID: withEndpoint, NetworkID: netID},
		}},
		statusByNetwork: map[uuid.UUID][]store.NetworkNodeStatus{netID: {
			{NetworkID: netID, NodeID: noEndpoint, ReconciliationStatus: "ready"},
			{NetworkID: netID, NodeID: withEndpoint, ReconciliationStatus: "ready"},
		}},
		nodes: []store.Node{
			{ID: noEndpoint, GatewayRole: true, Status: store.NodeStatusReady},
			{ID: withEndpoint, GatewayRole: true, Status: store.NodeStatusReady, IngressAdvertisedEndpoint: "https://gw.test:9444"},
		},
	}

	got, err := SelectGatewayForVM(context.Background(), f, vmID)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if got.ID != withEndpoint {
		t.Fatalf("selected gateway %s, want the one advertising an ingress endpoint %s", got.ID, withEndpoint)
	}
	if got.IngressAdvertisedEndpoint == "" {
		t.Errorf("selected gateway has no ingress endpoint: %+v", got)
	}
}

func TestSelectSkipsDeadGateway(t *testing.T) {
	netID := uuid.New()
	vmID := uuid.New()
	dead := uuid.New()
	f := &selectStoreFake{
		nicsByVM:    map[uuid.UUID][]store.VMNic{vmID: {{ID: uuid.New(), NetworkID: netID}}},
		networks:    map[uuid.UUID]store.Network{netID: {ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		memberships: map[uuid.UUID][]store.GatewayMembership{netID: {{GatewayID: dead, NetworkID: netID}}},
		statusByNetwork: map[uuid.UUID][]store.NetworkNodeStatus{netID: {
			{NetworkID: netID, NodeID: dead, ReconciliationStatus: "ready"},
		}},
		nodes: []store.Node{{ID: dead, GatewayRole: true, Status: store.NodeStatusUnreachable}},
	}

	_, err := SelectGatewayForVM(context.Background(), f, vmID)
	if !errors.Is(err, ErrIngressUnavailable) {
		t.Fatalf("err = %v, want ErrIngressUnavailable when the only ready member is unreachable", err)
	}
}
