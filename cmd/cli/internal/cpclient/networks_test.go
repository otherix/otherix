// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestCreateNetworkParamsBody locks the omitempty assembly: required
// fields are always present, optional fields appear only when the
// operator set them so the server applies its defaults otherwise.
func TestCreateNetworkParamsBody(t *testing.T) {
	mtu := 9000
	vlan := 42

	cases := []struct {
		name   string
		params CreateNetworkParams
		want   map[string]any
	}{
		{
			name: "minimal sets only required fields",
			params: CreateNetworkParams{
				Name:       "net-dev",
				Type:       "bridge",
				BridgeName: "br0",
			},
			want: map[string]any{
				"name":        "net-dev",
				"type":        "bridge",
				"bridge_name": "br0",
			},
		},
		{
			name: "managed nat with subnet",
			params: CreateNetworkParams{
				Name:       "net-nat",
				Type:       "bridge",
				BridgeName: "br-nat",
				Managed:    true,
				Egress:     "nat",
				Subnet:     "10.10.0.0/24",
				Gateway:    "10.10.0.1",
			},
			want: map[string]any{
				"name":        "net-nat",
				"type":        "bridge",
				"bridge_name": "br-nat",
				"managed":     true,
				"egress":      "nat",
				"subnet":      "10.10.0.0/24",
				"gateway":     "10.10.0.1",
			},
		},
		{
			name: "explicit mtu and vlan",
			params: CreateNetworkParams{
				Name:       "net-tagged",
				Type:       "bridge",
				BridgeName: "br1",
				Mtu:        &mtu,
				VlanTag:    &vlan,
			},
			want: map[string]any{
				"name":        "net-tagged",
				"type":        "bridge",
				"bridge_name": "br1",
				"mtu":         9000,
				"vlan_tag":    42,
			},
		},
		{
			name: "egress none is omitted",
			params: CreateNetworkParams{
				Name:       "net-none",
				Type:       "bridge",
				BridgeName: "br2",
				Egress:     "",
			},
			want: map[string]any{
				"name":        "net-none",
				"type":        "bridge",
				"bridge_name": "br2",
			},
		},
		{
			name: "overlay omits bridge_name",
			params: CreateNetworkParams{
				Name:   "net-overlay",
				Type:   "overlay",
				Subnet: "10.50.0.0/24",
			},
			want: map[string]any{
				"name":   "net-overlay",
				"type":   "overlay",
				"subnet": "10.50.0.0/24",
			},
		},
		{
			name: "dhcp true is present",
			params: CreateNetworkParams{
				Name:   "net-dhcp",
				Type:   "overlay",
				Egress: "nat",
				Subnet: "10.50.0.0/24",
				Dhcp:   true,
			},
			want: map[string]any{
				"name":   "net-dhcp",
				"type":   "overlay",
				"egress": "nat",
				"subnet": "10.50.0.0/24",
				"dhcp":   true,
			},
		},
		{
			name: "dhcp false is omitted",
			params: CreateNetworkParams{
				Name:       "net-nodhcp",
				Type:       "bridge",
				BridgeName: "br0",
				Dhcp:       false,
			},
			want: map[string]any{
				"name":        "net-nodhcp",
				"type":        "bridge",
				"bridge_name": "br0",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.params.body()
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("body() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCreateNetworkBodyIncludesDNS locks the dns plumbing: a non-nil
// *DNS transmits its value (including false, which is meaningful and
// must reach the server), while a nil *DNS omits the dns key so the
// server applies its default.
func TestCreateNetworkBodyIncludesDNS(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	cases := []struct {
		name   string
		params CreateNetworkParams
		want   map[string]any
	}{
		{
			name: "dns false is present",
			params: CreateNetworkParams{
				Name:   "net-overlay",
				Type:   "overlay",
				Subnet: "10.50.0.0/24",
				Dhcp:   true,
				DNS:    boolPtr(false),
			},
			want: map[string]any{
				"name":   "net-overlay",
				"type":   "overlay",
				"subnet": "10.50.0.0/24",
				"dhcp":   true,
				"dns":    false,
			},
		},
		{
			name: "nil dns omits the key",
			params: CreateNetworkParams{
				Name:   "net-overlay",
				Type:   "overlay",
				Subnet: "10.50.0.0/24",
				DNS:    nil,
			},
			want: map[string]any{
				"name":   "net-overlay",
				"type":   "overlay",
				"subnet": "10.50.0.0/24",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.params.body()
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("body() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
