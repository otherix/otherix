// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// TestNetworkToDeclared verifies the store.Network -> declaredNetwork
// projection, including netip pointer rendering for the nat case (subnet
// + gateway) and the nil case (plain managed bridge).
func TestNetworkToDeclared(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	prefix := netip.MustParsePrefix("10.10.0.0/24")
	gw := netip.MustParseAddr("10.10.0.1")
	subnetStr := "10.10.0.0/24"
	gwStr := "10.10.0.1"

	cases := []struct {
		name string
		in   store.Network
		want declaredNetwork
	}{
		{
			name: "nat network renders subnet and gateway",
			in: store.Network{
				ID:         id,
				Name:       "vmnat",
				Type:       store.NetworkTypeBridge,
				Managed:    true,
				Egress:     store.NetworkEgressNAT,
				BridgeName: "otx-vmnat",
				Mtu:        1500,
				Subnet:     &prefix,
				Gateway:    &gw,
			},
			want: declaredNetwork{
				ID:         id.String(),
				Name:       "vmnat",
				Type:       "bridge",
				Managed:    true,
				Egress:     "nat",
				BridgeName: "otx-vmnat",
				Mtu:        1500,
				Subnet:     &subnetStr,
				Gateway:    &gwStr,
			},
		},
		{
			name: "plain bridge leaves subnet and gateway nil",
			in: store.Network{
				ID:         id,
				Name:       "plain",
				Type:       store.NetworkTypeBridge,
				Managed:    true,
				Egress:     store.NetworkEgressNone,
				BridgeName: "br0",
				Mtu:        9000,
			},
			want: declaredNetwork{
				ID:         id.String(),
				Name:       "plain",
				Type:       "bridge",
				Managed:    true,
				Egress:     "none",
				BridgeName: "br0",
				Mtu:        9000,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := networkToDeclared(tc.in)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("networkToDeclared mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestNetworkToDeclaredCarriesVNI(t *testing.T) {
	vni := int32(1000)
	row := store.Network{
		ID:         uuid.New(),
		Name:       "ov",
		Type:       store.NetworkTypeOverlay,
		Managed:    true,
		Egress:     store.NetworkEgressNone,
		BridgeName: "otvb1000",
		Mtu:        store.OverlayMTU,
		VNI:        &vni,
	}
	dn := networkToDeclared(row)
	if dn.VNI == nil || *dn.VNI != 1000 {
		t.Errorf("declared VNI = %v, want 1000", dn.VNI)
	}
	if dn.Type != "overlay" {
		t.Errorf("declared Type = %q, want overlay", dn.Type)
	}
	// A bridge row carries no VNI.
	bridge := store.Network{ID: uuid.New(), Name: "br", Type: store.NetworkTypeBridge, BridgeName: "br0", Mtu: 1500}
	if got := networkToDeclared(bridge); got.VNI != nil {
		t.Errorf("bridge declared VNI = %v, want nil", got.VNI)
	}
}
