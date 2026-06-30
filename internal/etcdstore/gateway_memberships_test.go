// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// overlayNet creates an overlay network (VNI auto-allocated, subnet set) for the
// gateway-membership tests.
func overlayNet(t *testing.T, s interface {
	CreateNetwork(context.Context, store.CreateNetworkParams) (store.Network, error)
}, subnet string,
) store.Network {
	t.Helper()
	sn := netip.MustParsePrefix(subnet)
	n, err := s.CreateNetwork(context.Background(), store.CreateNetworkParams{
		ID:     uuid.New(),
		Name:   uniqueNetName("gw-ov"),
		Type:   store.NetworkTypeOverlay,
		Subnet: &sn,
		Config: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("CreateNetwork overlay: %v", err)
	}
	if n.VNI == nil {
		t.Fatalf("overlay network has nil VNI")
	}
	return n
}

// nicReservationKey reconstructs the shared per-(network, ip) VM-NIC reservation
// key so a test can seed a VM-held address or assert a gateway reservation.
func nicReservationKey(networkID uuid.UUID, ip netip.Addr) string {
	return etcd.Key("uniq", "vm_nics", "ipv4", networkID.String(), ip.String())
}

func TestCreateGatewayMembershipAllocatesIPAndMAC(t *testing.T) {
	s, raw := startStore(t)
	ctx := context.Background()
	net := overlayNet(t, s, "10.60.0.0/24")
	gw := uuid.New()

	m, err := s.CreateGatewayMembership(ctx, gw, net.ID)
	if err != nil {
		t.Fatalf("CreateGatewayMembership: %v", err)
	}
	if m.GatewayID != gw || m.NetworkID != net.ID {
		t.Errorf("membership ids = (%v, %v), want (%v, %v)", m.GatewayID, m.NetworkID, gw, net.ID)
	}
	if m.VNI != *net.VNI {
		t.Errorf("membership VNI = %d, want %d", m.VNI, *net.VNI)
	}
	if !strings.HasPrefix(m.MAC.String(), "52:54:00:") {
		t.Errorf("membership MAC = %q, want 52:54:00: prefix", m.MAC.String())
	}
	if !net.Subnet.Contains(m.TenantIP) {
		t.Errorf("membership TenantIP = %v, not inside subnet %v", m.TenantIP, net.Subnet)
	}
	if _, found, err := raw.Get(ctx, nicReservationKey(net.ID, m.TenantIP)); err != nil || !found {
		t.Errorf("reservation key for %v: found=%v err=%v, want found", m.TenantIP, found, err)
	}
}

func TestCreateGatewayMembershipSecondGatewayDifferentIP(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	net := overlayNet(t, s, "10.61.0.0/24")

	m1, err := s.CreateGatewayMembership(ctx, uuid.New(), net.ID)
	if err != nil {
		t.Fatalf("CreateGatewayMembership 1: %v", err)
	}
	m2, err := s.CreateGatewayMembership(ctx, uuid.New(), net.ID)
	if err != nil {
		t.Fatalf("CreateGatewayMembership 2: %v", err)
	}
	if m1.TenantIP == m2.TenantIP {
		t.Errorf("both gateways got %v, want distinct IPs", m1.TenantIP)
	}
}

func TestCreateGatewayMembershipSkipsVMHeldIP(t *testing.T) {
	s, raw := startStore(t)
	ctx := context.Background()
	net := overlayNet(t, s, "10.62.0.0/24")

	// Pre-reserve the lowest free host the way a VM NIC would, so the gateway
	// allocator must skip it - a gateway and a VM must never share an address.
	held := net.Subnet.Masked().Addr().Next()
	if err := raw.Put(ctx, nicReservationKey(net.ID, held), []byte(uuid.NewString())); err != nil {
		t.Fatalf("seed VM-NIC reservation: %v", err)
	}

	m, err := s.CreateGatewayMembership(ctx, uuid.New(), net.ID)
	if err != nil {
		t.Fatalf("CreateGatewayMembership: %v", err)
	}
	if m.TenantIP == held {
		t.Errorf("gateway collided on VM-held IP %v", held)
	}
}

// TestListGatewayMembershipsForNetworkAtRevPinsRows asserts the pinned-read path
// reads membership rows at the SAME revision as the per-network index. A
// membership that existed at the captured revision but was deleted afterward
// must still be listed when read at that revision: index and rows must reflect
// one consistent snapshot, never a torn read where the index lists a row the
// row-read then drops as not-found at current.
func TestListGatewayMembershipsForNetworkAtRevPinsRows(t *testing.T) {
	s, raw := startStore(t)
	ctx := context.Background()
	net := overlayNet(t, s, "10.64.0.0/24")
	gw := uuid.New()

	m, err := s.CreateGatewayMembership(ctx, gw, net.ID)
	if err != nil {
		t.Fatalf("CreateGatewayMembership: %v", err)
	}

	// Capture the store revision after the membership exists.
	_, capturedRev, err := raw.RangeRev(ctx, etcd.KeyPrefix, 0)
	if err != nil {
		t.Fatalf("capture revision: %v", err)
	}

	// Delete the membership AFTER the captured revision.
	if err := s.DeleteGatewayMembership(ctx, gw, net.ID); err != nil {
		t.Fatalf("DeleteGatewayMembership: %v", err)
	}

	// Read pinned to the captured revision: the membership existed then, so it
	// must still be returned even though it is gone at the current revision.
	got, err := s.ListGatewayMembershipsForNetworkAtRev(ctx, net.ID, capturedRev)
	if err != nil {
		t.Fatalf("ListGatewayMembershipsForNetworkAtRev: %v", err)
	}
	if len(got) != 1 || got[0].GatewayID != gw {
		t.Errorf("ListAtRev(%d) = %+v, want one row for gw %v", capturedRev, got, gw)
	}
	if len(got) == 1 && got[0].TenantIP != m.TenantIP {
		t.Errorf("ListAtRev TenantIP = %v, want %v", got[0].TenantIP, m.TenantIP)
	}

	// The latest read (rev==0) must still reflect the deletion.
	latest, err := s.ListGatewayMembershipsForNetwork(ctx, net.ID)
	if err != nil {
		t.Fatalf("ListGatewayMembershipsForNetwork: %v", err)
	}
	if len(latest) != 0 {
		t.Errorf("ListForNetwork latest = %+v, want empty after delete", latest)
	}
}

func TestGatewayMembershipListAndDelete(t *testing.T) {
	s, raw := startStore(t)
	ctx := context.Background()
	net := overlayNet(t, s, "10.63.0.0/24")
	gw := uuid.New()

	m, err := s.CreateGatewayMembership(ctx, gw, net.ID)
	if err != nil {
		t.Fatalf("CreateGatewayMembership: %v", err)
	}

	byNet, err := s.ListGatewayMembershipsForNetwork(ctx, net.ID)
	if err != nil {
		t.Fatalf("ListGatewayMembershipsForNetwork: %v", err)
	}
	if len(byNet) != 1 || byNet[0].GatewayID != gw {
		t.Errorf("ListForNetwork = %+v, want one row for gw %v", byNet, gw)
	}

	byGw, err := s.ListGatewayMembershipsForGateway(ctx, gw)
	if err != nil {
		t.Fatalf("ListGatewayMembershipsForGateway: %v", err)
	}
	if len(byGw) != 1 || byGw[0].NetworkID != net.ID {
		t.Errorf("ListForGateway = %+v, want one row for net %v", byGw, net.ID)
	}

	if err := s.DeleteGatewayMembership(ctx, gw, net.ID); err != nil {
		t.Fatalf("DeleteGatewayMembership: %v", err)
	}
	if _, found, err := raw.Get(ctx, nicReservationKey(net.ID, m.TenantIP)); err != nil || found {
		t.Errorf("reservation key after delete: found=%v err=%v, want gone", found, err)
	}
	byNet, err = s.ListGatewayMembershipsForNetwork(ctx, net.ID)
	if err != nil {
		t.Fatalf("ListGatewayMembershipsForNetwork after delete: %v", err)
	}
	if len(byNet) != 0 {
		t.Errorf("ListForNetwork after delete = %+v, want empty", byNet)
	}
}
