// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"net/netip"
	"testing"

	"github.com/otherix/otherix/internal/store"
)

func TestValidateNetworkType(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "bridge", input: "bridge", wantErr: false},
		{name: "overlay", input: "overlay", wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "unknown", input: "nat", wantErr: true},
		{name: "mesh", input: "mesh", wantErr: true},
		{name: "uppercase", input: "Bridge", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNetworkType(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateNetworkType(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateBridgeName(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "br0", input: "br0", wantErr: false},
		{name: "br-lan", input: "br-lan", wantErr: false},
		{name: "br_main", input: "br_main", wantErr: false},
		{name: "max len 15", input: "br" + "0123456789abc", wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "leading digit", input: "0br", wantErr: true},
		{name: "too long", input: "br" + "0123456789abcd", wantErr: true},
		{name: "invalid char", input: "br/0", wantErr: true},
		{name: "space", input: "br 0", wantErr: true},
		{name: "leading dash", input: "-br", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBridgeName(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateBridgeName(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateVLANTag(t *testing.T) {
	cases := []struct {
		name    string
		input   int
		wantErr bool
	}{
		{name: "min", input: 1, wantErr: false},
		{name: "max", input: 4094, wantErr: false},
		{name: "typical", input: 100, wantErr: false},
		{name: "zero reserved", input: 0, wantErr: true},
		{name: "4095 reserved", input: 4095, wantErr: true},
		{name: "negative", input: -1, wantErr: true},
		{name: "huge", input: 8192, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateVLANTag(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateVLANTag(%d) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateMTU(t *testing.T) {
	cases := []struct {
		name    string
		input   int
		wantErr bool
	}{
		{name: "min", input: MinMTU, wantErr: false},
		{name: "default", input: DefaultMTU, wantErr: false},
		{name: "vpn overlay", input: 1450, wantErr: false},
		{name: "jumbo", input: 9000, wantErr: false},
		{name: "max", input: MaxMTU, wantErr: false},
		{name: "below min", input: 67, wantErr: true},
		{name: "above max", input: MaxMTU + 1, wantErr: true},
		{name: "zero", input: 0, wantErr: true},
		{name: "negative", input: -1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMTU(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateMTU(%d) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateNetworkEgress(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "empty is none", input: "", wantErr: false},
		{name: "none", input: "none", wantErr: false},
		{name: "nat", input: "nat", wantErr: false},
		{name: "bridge not egress", input: "bridge", wantErr: true},
		{name: "garbage", input: "foo", wantErr: true},
		{name: "uppercase", input: "NAT", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNetworkEgress(tc.input)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateNetworkEgress(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestValidateNetworkInvariants(t *testing.T) {
	cases := []struct {
		name    string
		typ     store.NetworkType
		managed bool
		egress  store.NetworkEgress
		wantErr bool
	}{
		{name: "nat bridge managed ok", typ: store.NetworkTypeBridge, managed: true, egress: store.NetworkEgressNAT, wantErr: false},
		{name: "nat bridge unmanaged rejected", typ: store.NetworkTypeBridge, managed: false, egress: store.NetworkEgressNAT, wantErr: true},
		{name: "nat overlay managed ok", typ: store.NetworkTypeOverlay, managed: true, egress: store.NetworkEgressNAT, wantErr: false},
		{name: "nat overlay unmanaged rejected", typ: store.NetworkTypeOverlay, managed: false, egress: store.NetworkEgressNAT, wantErr: true},
		{name: "none bridge managed ok", typ: store.NetworkTypeBridge, managed: true, egress: store.NetworkEgressNone, wantErr: false},
		{name: "none bridge unmanaged ok", typ: store.NetworkTypeBridge, managed: false, egress: store.NetworkEgressNone, wantErr: false},
		{name: "empty egress no constraint", typ: store.NetworkType("overlay"), managed: false, egress: store.NetworkEgress(""), wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateNetworkInvariants(tc.typ, tc.managed, tc.egress)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateNetworkInvariants(%q, %v, %q) err = %v, wantErr = %v", tc.typ, tc.managed, tc.egress, err, tc.wantErr)
			}
		})
	}
}

func TestValidateDhcp(t *testing.T) {
	cases := []struct {
		name      string
		dhcp      bool
		hasSubnet bool
		typ       store.NetworkType
		wantErr   bool
	}{
		{name: "dhcp false bridge ok", dhcp: false, hasSubnet: false, typ: store.NetworkTypeBridge, wantErr: false},
		{name: "dhcp false overlay ok", dhcp: false, hasSubnet: false, typ: store.NetworkTypeOverlay, wantErr: false},
		{name: "dhcp true bridge rejected", dhcp: true, hasSubnet: true, typ: store.NetworkTypeBridge, wantErr: true},
		{name: "dhcp true overlay no egress with subnet ok", dhcp: true, hasSubnet: true, typ: store.NetworkTypeOverlay, wantErr: false},
		{name: "dhcp true overlay without subnet rejected", dhcp: true, hasSubnet: false, typ: store.NetworkTypeOverlay, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDhcp(tc.dhcp, tc.hasSubnet, tc.typ)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateDhcp(%v, %v, %q) err = %v, wantErr = %v", tc.dhcp, tc.hasSubnet, tc.typ, err, tc.wantErr)
			}
		})
	}
}

func TestValidateDNS(t *testing.T) {
	cases := []struct {
		name    string
		dns     bool
		typ     store.NetworkType
		wantErr bool
	}{
		{name: "dns false bridge ok", dns: false, typ: store.NetworkTypeBridge, wantErr: false},
		{name: "dns false overlay ok", dns: false, typ: store.NetworkTypeOverlay, wantErr: false},
		{name: "dns true overlay ok", dns: true, typ: store.NetworkTypeOverlay, wantErr: false},
		{name: "dns true bridge rejected", dns: true, typ: store.NetworkTypeBridge, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDNS(tc.dns, tc.typ)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateDNS(%v, %q) err = %v, wantErr = %v", tc.dns, tc.typ, err, tc.wantErr)
			}
		})
	}
}

func TestParseSubnet(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "canonical /24", input: "10.0.0.0/24", want: "10.0.0.0/24", wantErr: false},
		{name: "host bits dropped", input: "10.0.0.5/24", want: "10.0.0.0/24", wantErr: false},
		{name: "min prefix /8", input: "10.0.0.0/8", want: "10.0.0.0/8", wantErr: false},
		{name: "max prefix /30", input: "10.0.0.0/30", want: "10.0.0.0/30", wantErr: false},
		{name: "ipv6 rejected", input: "fd00::/64", wantErr: true},
		{name: "prefix too long /32", input: "10.0.0.0/32", wantErr: true},
		{name: "prefix too short /7", input: "10.0.0.0/7", wantErr: true},
		{name: "garbage", input: "not-a-subnet", wantErr: true},
		{name: "empty", input: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSubnet(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseSubnet(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got.String() != tc.want {
				t.Errorf("ParseSubnet(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestGatewayDefault(t *testing.T) {
	subnet := netip.MustParsePrefix("10.0.0.0/24")
	got := GatewayDefault(subnet)
	want := netip.MustParseAddr("10.0.0.1")
	if got != want {
		t.Errorf("GatewayDefault(%v) = %v, want %v", subnet, got, want)
	}
}

func TestValidateGatewayInSubnet(t *testing.T) {
	cases := []struct {
		name    string
		subnet  netip.Prefix
		gw      netip.Addr
		wantErr bool
	}{
		{name: "first host ok", subnet: netip.MustParsePrefix("10.0.0.0/24"), gw: netip.MustParseAddr("10.0.0.1"), wantErr: false},
		{name: "mid host ok", subnet: netip.MustParsePrefix("10.0.0.0/24"), gw: netip.MustParseAddr("10.0.0.42"), wantErr: false},
		{name: "network address rejected", subnet: netip.MustParsePrefix("10.0.0.0/24"), gw: netip.MustParseAddr("10.0.0.0"), wantErr: true},
		{name: "broadcast /24 rejected", subnet: netip.MustParsePrefix("10.0.0.0/24"), gw: netip.MustParseAddr("10.0.0.255"), wantErr: true},
		{name: "broadcast /30 rejected", subnet: netip.MustParsePrefix("10.0.0.0/30"), gw: netip.MustParseAddr("10.0.0.3"), wantErr: true},
		{name: "host in /30 ok", subnet: netip.MustParsePrefix("10.0.0.0/30"), gw: netip.MustParseAddr("10.0.0.1"), wantErr: false},
		{name: "outside subnet rejected", subnet: netip.MustParsePrefix("10.0.0.0/24"), gw: netip.MustParseAddr("10.1.0.1"), wantErr: true},
		{name: "ipv6 rejected", subnet: netip.MustParsePrefix("10.0.0.0/24"), gw: netip.MustParseAddr("fd00::1"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateGatewayInSubnet(tc.gw, tc.subnet)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateGatewayInSubnet(%v, %v) err = %v, wantErr = %v", tc.gw, tc.subnet, err, tc.wantErr)
			}
		})
	}
}

func TestValidateBridgeNameReservedPrefix(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool // true = expect error
	}{
		{name: "overlay bridge", in: "otb1000", want: true},
		{name: "vtep", in: "otvx5", want: true},
		{name: "wireguard", in: "otwg0", want: true},
		{name: "operator bridge", in: "vmbr0", want: false},
		{name: "plain", in: "br0", want: false},
		{name: "ot but not reserved", in: "oteth0", want: false},
		{name: "uppercase reserved", in: "OTB1000", want: true},
		{name: "mixed case vtep", in: "OtVx5", want: true},
		{name: "uppercase wireguard", in: "OTWG0", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBridgeName(tc.in)
			if (err != nil) != tc.want {
				t.Errorf("ValidateBridgeName(%q) err=%v, wantErr=%v", tc.in, err, tc.want)
			}
		})
	}
}
