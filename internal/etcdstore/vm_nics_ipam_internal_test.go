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

// TestAllocateNICIPv4 drives the allocator arithmetic directly: the first free
// host skips the network address, seeded reservations are honoured, and a full
// subnet (network + every host + broadcast taken) exhausts. It is the unit-level
// companion to the seam tests that drive allocation through the real bind txn.
func TestAllocateNICIPv4(t *testing.T) {
	s := startInternalStore(t)
	ctx := context.Background()

	netID := uuid.New()
	subnet := netip.MustParsePrefix("10.9.0.0/30")

	// First allocation skips the .0 network address and lands on .1.
	got, err := s.allocateNICIPv4(ctx, netID, subnet)
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
	if _, err := s.allocateNICIPv4(ctx, netID, subnet); !errors.Is(err, store.ErrSubnetExhausted) {
		t.Errorf("allocateNICIPv4(full /30) err = %v, want ErrSubnetExhausted", err)
	}

	// A wider subnet on a fresh network starts at .1.
	netID2 := uuid.New()
	subnet2 := netip.MustParsePrefix("10.62.0.0/24")
	got2, err := s.allocateNICIPv4(ctx, netID2, subnet2)
	if err != nil {
		t.Fatalf("allocateNICIPv4(/24) = %v", err)
	}
	if want := netip.MustParseAddr("10.62.0.1"); got2 != want {
		t.Errorf("allocateNICIPv4(/24) = %v, want %v", got2, want)
	}
}
