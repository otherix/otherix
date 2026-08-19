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

// declaredNetworksFake serves a fixed network list plus per-network NIC rows so
// loadDeclaredNetworks can be exercised end-to-end (network -> reservations) in
// the handler package. It embeds store.HeartbeatProjection (left nil) so any
// unexpected projection call panics.
type declaredNetworksFake struct {
	store.HeartbeatProjection
	networks []store.Network
	nics     map[uuid.UUID][]store.VMNic
	listed   []uuid.UUID // network ids ListVMNicsByNetwork was called for
}

func (f *declaredNetworksFake) ListNetworks(context.Context) ([]store.Network, error) {
	return f.networks, nil
}

func (f *declaredNetworksFake) ListVMNicsByNetwork(_ context.Context, networkID uuid.UUID) ([]store.VMNic, error) {
	f.listed = append(f.listed, networkID)
	return f.nics[networkID], nil
}

// NodeByID reports the heartbeating node as a plain hypervisor so the
// gateway-addr lookup short-circuits (a normal node owns no tenant gateway addr).
func (f *declaredNetworksFake) NodeByID(_ context.Context, id uuid.UUID) (store.Node, error) {
	return store.Node{ID: id}, nil
}

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("ParseMAC(%q): %v", s, err)
	}
	return mac
}

func addrPtr(t *testing.T, s string) *netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}
	return &a
}

// TestLoadDeclaredNetworksDHCP verifies the dhcp seam: a dhcp-enabled network
// surfaces dhcp=true plus exactly one reservation per NIC that has an
// Ipv4Address (NICs with nil Ipv4Address are skipped, reservations sorted by
// MAC), while a non-dhcp network surfaces dhcp=false and nil reservations
// without ever listing its NICs.
func TestLoadDeclaredNetworksDHCP(t *testing.T) {
	dhcpID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	plainID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	prefix := netip.MustParsePrefix("10.62.0.0/24")
	gw := netip.MustParseAddr("10.62.0.1")

	fake := &declaredNetworksFake{
		networks: []store.Network{
			{
				ID:          dhcpID,
				Name:        "vmdhcp",
				Type:        store.NetworkTypeBridge,
				Managed:     true,
				Egress:      store.NetworkEgressNAT,
				BridgeName:  "otx-vmdhcp",
				Mtu:         1500,
				Subnet:      &prefix,
				Gateway:     &gw,
				DhcpEnabled: true,
			},
			{
				ID:         plainID,
				Name:       "plain",
				Type:       store.NetworkTypeBridge,
				Managed:    true,
				Egress:     store.NetworkEgressNone,
				BridgeName: "br0",
				Mtu:        1500,
			},
		},
		nics: map[uuid.UUID][]store.VMNic{
			dhcpID: {
				{MacAddress: mustMAC(t, "52:54:00:00:00:02"), Ipv4Address: addrPtr(t, "10.62.0.6")},
				{MacAddress: mustMAC(t, "52:54:00:00:00:01"), Ipv4Address: addrPtr(t, "10.62.0.5")},
				{MacAddress: mustMAC(t, "52:54:00:00:00:03"), Ipv4Address: nil}, // unallocated -> skipped
			},
		},
	}

	h := newQuietHandler()
	got, err := h.loadDeclaredNetworks(context.Background(), fake, uuid.New())
	if err != nil {
		t.Fatalf("loadDeclaredNetworks: %v", err)
	}

	byID := make(map[string]declaredNetwork, len(got))
	for _, dn := range got {
		byID[dn.ID] = dn
	}

	dhcp := byID[dhcpID.String()]
	if !dhcp.DhcpEnabled {
		t.Errorf("dhcp network DhcpEnabled = false, want true")
	}
	wantRes := []dhcpReservation{
		{MAC: "52:54:00:00:00:01", IP: "10.62.0.5"},
		{MAC: "52:54:00:00:00:02", IP: "10.62.0.6"},
	}
	if diff := cmp.Diff(wantRes, dhcp.Reservations); diff != "" {
		t.Errorf("dhcp reservations mismatch (-want +got):\n%s", diff)
	}

	plain := byID[plainID.String()]
	if plain.DhcpEnabled {
		t.Errorf("plain network DhcpEnabled = true, want false")
	}
	if plain.Reservations != nil {
		t.Errorf("plain network Reservations = %v, want nil", plain.Reservations)
	}

	// The NIC join runs only for the dhcp network.
	if diff := cmp.Diff([]uuid.UUID{dhcpID}, fake.listed); diff != "" {
		t.Errorf("ListVMNicsByNetwork called for unexpected networks (-want +got):\n%s", diff)
	}
}

// TestNetworkToDeclaredCarriesDNS verifies the projection copies DNSEnabled onto
// the declared-network wire shape so the agent reconciler can read it.
func TestNetworkToDeclaredCarriesDNS(t *testing.T) {
	dn := networkToDeclared(store.Network{
		ID:          uuid.New(),
		Name:        "n",
		Type:        store.NetworkTypeOverlay,
		Egress:      store.NetworkEgressNone,
		DhcpEnabled: true,
		DNSEnabled:  true,
	})
	if !dn.DNSEnabled {
		t.Errorf("declared DNSEnabled = false, want true")
	}
}

// VMSoftDeleted answers "not deleted" for every id: these tests do not drive
// the heartbeat teardown path. A test that does asserts on its own answer.
func (f *declaredNetworksFake) VMSoftDeleted(context.Context, uuid.UUID) (bool, string, error) {
	return false, "", nil
}
