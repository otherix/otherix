// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package dhcp4

import (
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
)

func discardResponder(t *testing.T) *responder {
	t.Helper()
	r, err := New(Config{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return r
}

func TestLookupByMAC_HitReturnsReservationIP(t *testing.T) {
	r := discardResponder(t)
	mac, err := net.ParseMAC("52:54:00:11:22:33")
	if err != nil {
		t.Fatalf("ParseMAC error = %v", err)
	}
	ip := netip.MustParseAddr("10.42.0.7")
	if err := r.RegisterNetwork(NetworkConfig{
		NetworkID:    "net-1",
		Bridge:       "otvb100",
		Subnet:       netip.MustParsePrefix("10.42.0.0/24"),
		Reservations: []Reservation{{MAC: mac, IP: ip}},
	}); err != nil {
		t.Fatalf("RegisterNetwork error = %v", err)
	}

	// Lookup tolerates any textual MAC form (it canonicalizes via net.ParseMAC).
	got, ok := r.LookupByMAC("52:54:00:11:22:33")
	if !ok {
		t.Fatalf("LookupByMAC(known) ok = false, want true")
	}
	if got != ip {
		t.Errorf("LookupByMAC(known) = %v, want %v", got, ip)
	}
}

func TestLookupByMAC_UnknownMACMisses(t *testing.T) {
	r := discardResponder(t)
	mac, _ := net.ParseMAC("52:54:00:11:22:33")
	if err := r.RegisterNetwork(NetworkConfig{
		NetworkID:    "net-1",
		Bridge:       "otvb100",
		Subnet:       netip.MustParsePrefix("10.42.0.0/24"),
		Reservations: []Reservation{{MAC: mac, IP: netip.MustParseAddr("10.42.0.7")}},
	}); err != nil {
		t.Fatalf("RegisterNetwork error = %v", err)
	}

	if _, ok := r.LookupByMAC("52:54:00:aa:bb:cc"); ok {
		t.Errorf("LookupByMAC(unknown) ok = true, want false")
	}
	if _, ok := r.LookupByMAC("not-a-mac"); ok {
		t.Errorf("LookupByMAC(malformed) ok = true, want false")
	}
}
