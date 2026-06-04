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
	"time"

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

// TestCreateNetworkOverlayRefusesSubFloorUnderlay seeds a sub-floor underlay MTU
// directly on the singleton (bypassing SeedUnderlayMTU, which rejects below-floor
// values) to model a legacy cluster seeded under the old 1280 floor. Creating a
// type=overlay network must refuse with *store.UnderlayBelowFloorError rather than
// stamp a derived overlay MTU below the 1280-byte IPv6 minimum link MTU.
func TestCreateNetworkOverlayRefusesSubFloorUnderlay(t *testing.T) {
	s := startInternalStore(t)
	ctx := context.Background()

	// Seed underlay MTU = 1300 directly (sub-floor; SeedUnderlayMTU would reject it).
	const subFloor int32 = 1300
	if err := s.casClusterSettings(ctx, func(cs *store.ClusterSetting) {
		v := subFloor
		cs.UnderlayMTU = &v
	}); err != nil {
		t.Fatalf("seed sub-floor underlay: %v", err)
	}

	sn := netip.MustParsePrefix("10.50.0.0/24")
	_, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: uuid.New(), Name: "ov-subfloor", Type: store.NetworkTypeOverlay,
		Subnet: &sn, Config: []byte("{}"),
	})
	var floorErr *store.UnderlayBelowFloorError
	if !errors.As(err, &floorErr) {
		t.Fatalf("CreateNetwork err = %v, want *store.UnderlayBelowFloorError", err)
	}
	if floorErr.UnderlayMTU != subFloor {
		t.Errorf("UnderlayMTU = %d, want %d", floorErr.UnderlayMTU, subFloor)
	}
	if want := subFloor - store.OverlayEncapOverhead; floorErr.DerivedOverlayMTU != want {
		t.Errorf("DerivedOverlayMTU = %d, want %d", floorErr.DerivedOverlayMTU, want)
	}
	if floorErr.MinUnderlayMTU != MinUnderlayMTU {
		t.Errorf("MinUnderlayMTU = %d, want %d", floorErr.MinUnderlayMTU, MinUnderlayMTU)
	}
}

// TestCreateNetworkOverlayAllowsAtFloorUnderlay confirms the guard admits an
// underlay MTU exactly at the floor (1390), which derives an overlay MTU of 1280
// (the IPv6 minimum).
func TestCreateNetworkOverlayAllowsAtFloorUnderlay(t *testing.T) {
	s := startInternalStore(t)
	ctx := context.Background()

	if err := s.SeedUnderlayMTU(ctx, int(MinUnderlayMTU)); err != nil {
		t.Fatalf("seed at-floor underlay: %v", err)
	}
	sn := netip.MustParsePrefix("10.50.0.0/24")
	n, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: uuid.New(), Name: "ov-atfloor", Type: store.NetworkTypeOverlay,
		Subnet: &sn, Config: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("CreateNetwork at floor = %v, want success", err)
	}
	if want := MinUnderlayMTU - store.OverlayEncapOverhead; n.Mtu != want {
		t.Errorf("overlay Mtu = %d, want %d", n.Mtu, want)
	}
}

// TestDeleteNetworkIgnoresStaleNicIndexWithoutRow reproduces the hard-gone NIC
// wedge: a per-network vm_nic index entry survives while its NIC row is gone
// (the hard-gone branch of vmNicDeleteOps can only drop the per-VM index, never
// the per-network one, because it cannot reconstruct NetworkID from a missing
// row). The stale index entry must NOT block DeleteNetwork - countVMNicsOnNetwork
// reconciles against live NIC rows. Against the old len-of-index code this test
// fails (the delete is wedged with a phantom vm_nics blocker).
func TestDeleteNetworkIgnoresStaleNicIndexWithoutRow(t *testing.T) {
	s := startInternalStore(t)
	ctx := context.Background()

	netID := uuid.New()
	if _, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: netID, Name: "stale", Type: store.NetworkTypeBridge,
		BridgeName: "br0", Mtu: 1500, Config: []byte(`{}`),
	}); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}

	// Seed a per-network index entry whose NIC row does not exist (hard-gone).
	staleNicID := uuid.New()
	if err := s.c.Put(ctx, vmNicNetworkIndexKey(netID, staleNicID), []byte(staleNicID.String())); err != nil {
		t.Fatalf("seed stale network index: %v", err)
	}
	if _, found, err := s.c.Get(ctx, vmNicKey(staleNicID)); err != nil || found {
		t.Fatalf("stale NIC row unexpectedly present (found=%v err=%v)", found, err)
	}

	// The blocking count must skip the stale entry: zero live referents.
	n, err := s.countVMNicsOnNetwork(ctx, netID)
	if err != nil {
		t.Fatalf("countVMNicsOnNetwork: %v", err)
	}
	if n != 0 {
		t.Errorf("countVMNicsOnNetwork = %d, want 0 (stale index entry must not count)", n)
	}

	// DeleteNetwork must succeed rather than wedge on the phantom blocker.
	if err := s.DeleteNetwork(ctx, netID); err != nil {
		t.Fatalf("DeleteNetwork wedged by stale nic index = %v, want nil", err)
	}
}

// TestDeleteNetworkBlocksOnLiveNicRow is the discriminating counterpart: a
// per-network index entry whose NIC row IS live and not soft-deleted must still
// block DeleteNetwork. Guards against the reconcile over-counting toward zero.
func TestDeleteNetworkBlocksOnLiveNicRow(t *testing.T) {
	s := startInternalStore(t)
	ctx := context.Background()

	netID := uuid.New()
	if _, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: netID, Name: "live", Type: store.NetworkTypeBridge,
		BridgeName: "br0", Mtu: 1500, Config: []byte(`{}`),
	}); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}

	now := time.Now().UTC()
	nicID := uuid.New()
	nic := store.VMNic{
		ID: nicID, VmID: uuid.New(), NetworkID: netID, DeviceOrder: 0,
		Model: store.NicModelVirtio, Generation: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.c.PutJSON(ctx, vmNicKey(nicID), nic); err != nil {
		t.Fatalf("seed live nic row: %v", err)
	}
	if err := s.c.Put(ctx, vmNicNetworkIndexKey(netID, nicID), []byte(nicID.String())); err != nil {
		t.Fatalf("seed network index: %v", err)
	}

	n, err := s.countVMNicsOnNetwork(ctx, netID)
	if err != nil {
		t.Fatalf("countVMNicsOnNetwork: %v", err)
	}
	if n != 1 {
		t.Errorf("countVMNicsOnNetwork = %d, want 1 (live referent)", n)
	}

	err = s.DeleteNetwork(ctx, netID)
	var blocking *store.ResourceInUseError
	if !errors.As(err, &blocking) {
		t.Fatalf("DeleteNetwork err = %v, want *store.ResourceInUseError", err)
	}
	if blocking.Resources["vm_nics"] != 1 {
		t.Errorf("blocking vm_nics = %d, want 1", blocking.Resources["vm_nics"])
	}
}
