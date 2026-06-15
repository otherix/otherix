// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package netfabric

import (
	"net"
	"net/netip"
	"testing"
)

func TestBuildGratuitousARP(t *testing.T) {
	mac, _ := net.ParseMAC("52:54:00:12:34:56")
	ip := netip.MustParseAddr("10.42.0.7")

	frame, err := buildGratuitousARP(mac, ip)
	if err != nil {
		t.Fatalf("buildGratuitousARP: %v", err)
	}
	if got := frame[0:6]; !equalBytes(got, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}) {
		t.Errorf("eth dst = %x, want broadcast", got)
	}
	if got := frame[6:12]; !equalBytes(got, mac) {
		t.Errorf("eth src = %x, want %x", got, mac)
	}
	if got := frame[12:14]; !equalBytes(got, []byte{0x08, 0x06}) {
		t.Errorf("ethertype = %x, want 0806", got)
	}
	if got := frame[20:22]; !equalBytes(got, []byte{0x00, 0x02}) {
		t.Errorf("arp opcode = %x, want 0002 (reply)", got)
	}
	if got := frame[22:28]; !equalBytes(got, mac) {
		t.Errorf("arp SHA = %x, want %x", got, mac)
	}
	want4 := ip.As4()
	if got := frame[28:32]; !equalBytes(got, want4[:]) {
		t.Errorf("arp SPA = %x, want %x", got, want4)
	}
	if got := frame[38:42]; !equalBytes(got, want4[:]) {
		t.Errorf("arp TPA = %x, want %x", got, want4)
	}
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
