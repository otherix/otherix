// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package netfabric

import "testing"

func TestGatewayMAC(t *testing.T) {
	// Locally-administered + unicast: first octet 0x02; per-VNI in the low 3 octets.
	got := GatewayMAC(0x0100EF) // 65775
	want := "02:00:00:01:00:ef"
	if got.String() != want {
		t.Errorf("GatewayMAC(0x0100EF) = %v, want %v", got.String(), want)
	}
	if got[0]&0x02 == 0 {
		t.Errorf("GatewayMAC first octet %#x is not locally administered", got[0])
	}
	if got[0]&0x01 != 0 {
		t.Errorf("GatewayMAC first octet %#x is multicast, want unicast", got[0])
	}
	// Per-VNI distinct.
	if GatewayMAC(1).String() == GatewayMAC(2).String() {
		t.Errorf("GatewayMAC must differ per vni")
	}
}

func TestGatewayMACFromID_DeterministicAndLocallyAdministered(t *testing.T) {
	a := GatewayMACFromID("net-1234")
	b := GatewayMACFromID("net-1234")
	if a.String() != b.String() {
		t.Errorf("GatewayMACFromID not deterministic: %s vs %s", a, b)
	}
	if a[0]&0x02 == 0 {
		t.Errorf("MAC %s is not locally-administered (bit 0x02 unset in first octet)", a)
	}
	if a[0]&0x01 != 0 {
		t.Errorf("MAC %s is multicast (bit 0x01 set in first octet), want unicast", a)
	}
	c := GatewayMACFromID("net-9999")
	if a.String() == c.String() {
		t.Errorf("distinct ids produced the same MAC: %s", a)
	}
}

func TestOverlayGatewayAddr(t *testing.T) {
	if got := OverlayGatewayAddr.String(); got != "169.254.1.1" {
		t.Errorf("OverlayGatewayAddr = %v, want 169.254.1.1", got)
	}
	if !OverlayGatewayAddr.Is4() {
		t.Errorf("OverlayGatewayAddr must be IPv4")
	}
}
