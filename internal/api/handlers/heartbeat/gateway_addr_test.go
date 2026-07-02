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

// fakeGatewayAddrProjection serves loadDeclaredNetworks a fixed network set, a
// per-node Kind, and a per-gateway membership set.
type fakeGatewayAddrProjection struct {
	store.HeartbeatProjection
	networks    []store.Network
	kindByNode  map[uuid.UUID]string
	memberships map[uuid.UUID][]store.GatewayMembership
}

func (f *fakeGatewayAddrProjection) ListNetworks(context.Context) ([]store.Network, error) {
	return f.networks, nil
}

func (f *fakeGatewayAddrProjection) NodeByID(_ context.Context, id uuid.UUID) (store.Node, error) {
	return store.Node{ID: id, GatewayRole: f.kindByNode[id] == store.NodeKindGateway}, nil
}

func (f *fakeGatewayAddrProjection) ListGatewayMembershipsForGateway(_ context.Context, gatewayID uuid.UUID) ([]store.GatewayMembership, error) {
	return f.memberships[gatewayID], nil
}

func overlayStoreNetwork(id uuid.UUID) store.Network {
	v := int32(1000)
	return store.Network{
		ID:         id,
		Name:       "ov",
		Type:       store.NetworkTypeOverlay,
		Managed:    true,
		Egress:     store.NetworkEgressNone,
		BridgeName: "otvb1000",
		Mtu:        1390,
		VNI:        &v,
	}
}

// TestLoadDeclaredNetworksGatewayGetsAddr verifies a gateway-kind node that
// holds a membership on an overlay network receives the tenant IP + unicast MAC
// on that network's declared_networks entry.
func TestLoadDeclaredNetworksGatewayGetsAddr(t *testing.T) {
	gwNode := uuid.New()
	netID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	mac, err := net.ParseMAC("52:54:00:ab:cd:ef")
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	hp := &fakeGatewayAddrProjection{
		networks:   []store.Network{overlayStoreNetwork(netID)},
		kindByNode: map[uuid.UUID]string{gwNode: store.NodeKindGateway},
		memberships: map[uuid.UUID][]store.GatewayMembership{
			gwNode: {{
				GatewayID: gwNode,
				NetworkID: netID,
				VNI:       1000,
				MAC:       mac,
				TenantIP:  netip.MustParseAddr("10.50.0.7"),
			}},
		},
	}
	h := &Handler{log: discardLogger()}
	got, err := h.loadDeclaredNetworks(context.Background(), hp, gwNode)
	if err != nil {
		t.Fatalf("loadDeclaredNetworks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("declared networks = %d, want 1", len(got))
	}
	want := &gatewayAddr{IP: "10.50.0.7", MAC: "52:54:00:ab:cd:ef"}
	if diff := cmp.Diff(want, got[0].GatewayAddr); diff != "" {
		t.Errorf("gateway_addr mismatch (-want +got):\n%s", diff)
	}
}

// TestLoadDeclaredNetworksNormalNodeGetsNoAddr verifies a hypervisor node with a
// NIC on the same overlay network receives a nil gateway_addr: only a gateway
// recipient owns the tenant IP.
func TestLoadDeclaredNetworksNormalNodeGetsNoAddr(t *testing.T) {
	normalNode := uuid.New()
	netID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	hp := &fakeGatewayAddrProjection{
		networks:   []store.Network{overlayStoreNetwork(netID)},
		kindByNode: map[uuid.UUID]string{normalNode: store.NodeKindNode},
	}
	h := &Handler{log: discardLogger()}
	got, err := h.loadDeclaredNetworks(context.Background(), hp, normalNode)
	if err != nil {
		t.Fatalf("loadDeclaredNetworks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("declared networks = %d, want 1", len(got))
	}
	if got[0].GatewayAddr != nil {
		t.Errorf("normal node got gateway_addr %+v, want nil", got[0].GatewayAddr)
	}
}
