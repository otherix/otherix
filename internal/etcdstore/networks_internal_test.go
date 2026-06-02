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

// TestDeleteNetworkElseBranchPreservesForeignGuard forces the DeleteNetwork
// Else branch: the row is live (so NetworkByID passes the front door) but the
// name guard has been re-pointed to a DIFFERENT network id (simulating a
// concurrent same-name re-create that won the guard). The delete must
// soft-delete the row + drop the VNI guard but MUST NOT delete the
// foreign-owned name guard. Against the old unconditional-delete code this test
// fails (the guard is nuked).
//
// This is the discriminating counterpart to the external-package
// TestDeleteNetworkLeavesReusedNameGuardIntact, which asserts the end-state
// invariant but cannot reach the Else branch single-threaded (NetworkByID's
// deleted-check returns ErrNotFound before the txn runs). Here X stays live and
// the foreign guard is seeded directly, so the txn's Else branch is genuinely
// exercised.
func TestDeleteNetworkElseBranchPreservesForeignGuard(t *testing.T) {
	s := startInternalStore(t)
	ctx := context.Background()

	// Create X as an OVERLAY so the delete also drops a VNI guard: the Else
	// branch must still perform the id/VNI-keyed cleanup while sparing the
	// foreign name guard.
	sn := netip.MustParsePrefix("10.50.0.0/24")
	x, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: uuid.New(), Name: "foo", Type: store.NetworkTypeOverlay, Subnet: &sn, Config: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("create X: %v", err)
	}
	if x.VNI == nil {
		t.Fatalf("overlay X did not allocate a VNI")
	}

	// Sanity: the name guard and VNI guard exist and point at X.
	if v, found, err := s.c.Get(ctx, networkNameGuard("foo")); err != nil || !found || string(v) != x.ID.String() {
		t.Fatalf("seeded name guard = (%q found=%v err=%v), want X id %s", v, found, err, x.ID)
	}
	if _, found, err := s.c.Get(ctx, networkVNIGuard(*x.VNI)); err != nil || !found {
		t.Fatalf("VNI guard missing before delete (found=%v err=%v)", found, err)
	}

	// Re-point the name guard at a DIFFERENT network id, simulating a concurrent
	// same-name re-create that won the guard while X is still live.
	foreign := uuid.New()
	if err := s.c.Put(ctx, networkNameGuard("foo"), []byte(foreign.String())); err != nil {
		t.Fatalf("repoint name guard to foreign: %v", err)
	}

	// Delete X. The guard no longer points at X.ID, so the txn takes the Else
	// branch: soft-delete the row + drop the VNI guard, leave the name guard.
	if err := s.DeleteNetwork(ctx, x.ID); err != nil {
		t.Fatalf("DeleteNetwork (Else branch): %v", err)
	}

	// Row was soft-deleted (front door now reports it gone).
	if _, err := s.NetworkByID(ctx, x.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("NetworkByID after delete = %v, want store.ErrNotFound", err)
	}

	// The foreign-owned name guard SURVIVED the delete. The old
	// unconditional-delete code would have nuked it here.
	v, found, err := s.c.Get(ctx, networkNameGuard("foo"))
	if err != nil {
		t.Fatalf("Get name guard after delete: %v", err)
	}
	if !found {
		t.Errorf("foreign name guard deleted, want preserved (Else branch must not touch a re-pointed guard)")
	}
	if string(v) != foreign.String() {
		t.Errorf("name guard value = %q, want foreign %s (guard clobbered)", v, foreign)
	}

	// The Else branch still drops X's own VNI guard (id/VNI-keyed cleanup).
	if _, found, err := s.c.Get(ctx, networkVNIGuard(*x.VNI)); err != nil || found {
		t.Errorf("VNI guard still present after delete (found=%v err=%v), want dropped", found, err)
	}
}
