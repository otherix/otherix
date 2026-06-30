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
			{ID: ready, Kind: store.NodeKindGateway, Status: store.NodeStatusReady},
			{ID: pending, Kind: store.NodeKindGateway, Status: store.NodeStatusReady},
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
			{ID: g1, Kind: store.NodeKindGateway, Status: store.NodeStatusReady},
			{ID: g2, Kind: store.NodeKindGateway, Status: store.NodeStatusReady},
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

func TestSelectSkipsGoneGateway(t *testing.T) {
	netID := uuid.New()
	vmID := uuid.New()
	gone := uuid.New()
	f := &selectStoreFake{
		nicsByVM:    map[uuid.UUID][]store.VMNic{vmID: {{ID: uuid.New(), NetworkID: netID}}},
		networks:    map[uuid.UUID]store.Network{netID: {ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		memberships: map[uuid.UUID][]store.GatewayMembership{netID: {{GatewayID: gone, NetworkID: netID}}},
		statusByNetwork: map[uuid.UUID][]store.NetworkNodeStatus{netID: {
			{NetworkID: netID, NodeID: gone, ReconciliationStatus: "ready"},
		}},
		nodes: []store.Node{{ID: gone, Kind: store.NodeKindGateway, Status: store.NodeStatusGone}},
	}

	_, err := SelectGatewayForVM(context.Background(), f, vmID)
	if !errors.Is(err, ErrIngressUnavailable) {
		t.Fatalf("err = %v, want ErrIngressUnavailable when the only ready member is gone", err)
	}
}
