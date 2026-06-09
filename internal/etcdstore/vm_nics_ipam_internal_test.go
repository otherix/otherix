// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// TestAllocateNICIPv4 drives the allocator arithmetic directly: with a nil
// gateway (the overlay reality, where the gateway is link-local and not in the
// subnet) the first free host skips only the network address and lands on .1;
// seeded reservations are honoured; and a full subnet (network + every host +
// broadcast taken) exhausts. It is the unit-level companion to the seam tests
// that drive allocation through the real bind txn.
func TestAllocateNICIPv4(t *testing.T) {
	s := startInternalStore(t)
	ctx := context.Background()

	netID := uuid.New()
	subnet := netip.MustParsePrefix("10.9.0.0/30")

	// First allocation (nil gateway) skips the .0 network address and lands
	// on .1 - .1 is a valid VM address because the overlay gateway is
	// link-local, not in this subnet.
	got, err := s.allocateNICIPv4(ctx, netID, subnet, nil)
	if err != nil {
		t.Fatalf("allocateNICIPv4(empty) = %v", err)
	}
	if want := netip.MustParseAddr("10.9.0.1"); got != want {
		t.Errorf("allocateNICIPv4(empty) = %v, want %v", got, want)
	}

	// Seed reservations for .1 and .2; the only remaining address .3 is the
	// broadcast, so the next allocation exhausts.
	for _, ip := range []string{"10.9.0.1", "10.9.0.2"} {
		key := vmNicIPv4ReservationKey(netID, netip.MustParseAddr(ip))
		if err := s.c.Put(ctx, key, []byte(uuid.NewString())); err != nil {
			t.Fatalf("seed reservation %s: %v", ip, err)
		}
	}
	if _, err := s.allocateNICIPv4(ctx, netID, subnet, nil); !errors.Is(err, store.ErrSubnetExhausted) {
		t.Errorf("allocateNICIPv4(full /30) err = %v, want ErrSubnetExhausted", err)
	}

	// A wider subnet on a fresh network with a nil gateway starts at .1.
	netID2 := uuid.New()
	subnet2 := netip.MustParsePrefix("10.62.0.0/24")
	got2, err := s.allocateNICIPv4(ctx, netID2, subnet2, nil)
	if err != nil {
		t.Fatalf("allocateNICIPv4(/24) = %v", err)
	}
	if want := netip.MustParseAddr("10.62.0.1"); got2 != want {
		t.Errorf("allocateNICIPv4(/24) = %v, want %v", got2, want)
	}

	// An in-subnet gateway (bridge-style network) is never handed out: with
	// gateway 10.62.0.1 the first free host is .2, not .1.
	netID3 := uuid.New()
	gw := netip.MustParseAddr("10.62.0.1")
	got3, err := s.allocateNICIPv4(ctx, netID3, subnet2, &gw)
	if err != nil {
		t.Fatalf("allocateNICIPv4(/24, gw .1) = %v", err)
	}
	if want := netip.MustParseAddr("10.62.0.2"); got3 != want {
		t.Errorf("allocateNICIPv4(/24, gw .1) = %v, want %v (gateway skipped)", got3, want)
	}
}
