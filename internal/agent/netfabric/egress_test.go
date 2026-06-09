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

func TestOverlayGatewayAddr(t *testing.T) {
	if got := OverlayGatewayAddr.String(); got != "169.254.1.1" {
		t.Errorf("OverlayGatewayAddr = %v, want 169.254.1.1", got)
	}
	if !OverlayGatewayAddr.Is4() {
		t.Errorf("OverlayGatewayAddr must be IPv4")
	}
}
