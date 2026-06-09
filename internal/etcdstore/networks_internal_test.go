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

// TestNetworkDhcpEnabledRoundTrips creates a network with DhcpEnabled=true,
// confirms CreateNetwork returns it set, that NetworkByID re-reads it as true
// (round-trip through the etcd JSON), and that UpdateNetwork (which never
// carries the immutable flag) preserves it while mutating another field.
func TestNetworkDhcpEnabledRoundTrips(t *testing.T) {
	s := startInternalStore(t)
	ctx := context.Background()

	sn := netip.MustParsePrefix("10.60.0.0/24")
	netID := uuid.New()
	created, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: netID, Name: "dhcp-net", Type: store.NetworkTypeBridge,
		BridgeName: "br0", Mtu: 1500, Subnet: &sn, DhcpEnabled: true, Config: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if !created.DhcpEnabled {
		t.Errorf("CreateNetwork returned DhcpEnabled = false, want true")
	}

	reread, err := s.NetworkByID(ctx, netID)
	if err != nil {
		t.Fatalf("NetworkByID: %v", err)
	}
	if !reread.DhcpEnabled {
		t.Errorf("NetworkByID DhcpEnabled = false, want true (round-trip through etcd JSON)")
	}

	// UpdateNetwork carries no DhcpEnabled (immutable); changing Name must
	// preserve the flag because updated := existing copies it forward.
	updated, err := s.UpdateNetwork(ctx, store.UpdateNetworkParams{
		ID: netID, Name: "dhcp-net-renamed", BridgeName: "br0", Mtu: 1500,
		Subnet: &sn, Config: []byte("{}"),
	})
	if err != nil {
		t.Fatalf("UpdateNetwork: %v", err)
	}
	if !updated.DhcpEnabled {
		t.Errorf("UpdateNetwork DhcpEnabled = false, want true (immutable field must be preserved)")
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

	// Capture the VNI sequence counter before the refused create. The floor
	// check must run BEFORE allocateVNI -> nextSeq, so a refused create must
	// leave this counter untouched (no VNI burned from the 24-bit space).
	vniSeqBefore, _, err := s.c.Get(ctx, networkVNISeqKey())
	if err != nil {
		t.Fatalf("read VNI seq before: %v", err)
	}

	sn := netip.MustParsePrefix("10.50.0.0/24")
	_, err = s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: uuid.New(), Name: "ov-subfloor", Type: store.NetworkTypeOverlay,
		Subnet: &sn, Config: []byte("{}"),
	})
	var floorErr *store.UnderlayBelowFloorError
	if !errors.As(err, &floorErr) {
		t.Fatalf("CreateNetwork err = %v, want *store.UnderlayBelowFloorError", err)
	}

	// The refused create must NOT have advanced the VNI sequence counter:
	// the floor check is hoisted above allocateVNI, so no VNI is burned.
	vniSeqAfter, _, err := s.c.Get(ctx, networkVNISeqKey())
	if err != nil {
		t.Fatalf("read VNI seq after: %v", err)
	}
	if got, want := string(vniSeqAfter), string(vniSeqBefore); got != want {
		t.Errorf("VNI seq counter = %q after refused create, want %q (unchanged); a refused sub-floor overlay create must burn no VNI", got, want)
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

// TestListVMNicsByNetwork seeds two live NICs on a network (one with an
// Ipv4Address, one without), a soft-deleted NIC, and a stale index entry whose
// row is gone, then asserts ListVMNicsByNetwork returns exactly the two live
// rows: the soft-deleted NIC is excluded and the stale index entry is skipped.
func TestListVMNicsByNetwork(t *testing.T) {
	s := startInternalStore(t)
	ctx := context.Background()

	netID := uuid.New()
	if _, err := s.CreateNetwork(ctx, store.CreateNetworkParams{
		ID: netID, Name: "dhcp-list", Type: store.NetworkTypeBridge,
		BridgeName: "br0", Mtu: 1500, Subnet: ptrPrefix("10.70.0.0/24"),
		DhcpEnabled: true, Config: []byte(`{}`),
	}); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}

	now := time.Now().UTC()

	// Live NIC with an Ipv4Address.
	ip := netip.MustParseAddr("10.70.0.10")
	withIP := uuid.New()
	if err := s.c.PutJSON(ctx, vmNicKey(withIP), store.VMNic{
		ID: withIP, VmID: uuid.New(), NetworkID: netID, DeviceOrder: 0,
		Model: store.NicModelVirtio, Ipv4Address: &ip, Generation: 1,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed nic with ip: %v", err)
	}
	if err := s.c.Put(ctx, vmNicNetworkIndexKey(netID, withIP), []byte(withIP.String())); err != nil {
		t.Fatalf("seed index withIP: %v", err)
	}

	// Live NIC without an Ipv4Address.
	noIP := uuid.New()
	if err := s.c.PutJSON(ctx, vmNicKey(noIP), store.VMNic{
		ID: noIP, VmID: uuid.New(), NetworkID: netID, DeviceOrder: 1,
		Model: store.NicModelVirtio, Generation: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed nic without ip: %v", err)
	}
	if err := s.c.Put(ctx, vmNicNetworkIndexKey(netID, noIP), []byte(noIP.String())); err != nil {
		t.Fatalf("seed index noIP: %v", err)
	}

	// Soft-deleted NIC: indexed but excluded.
	deletedID := uuid.New()
	if err := s.c.PutJSON(ctx, vmNicKey(deletedID), store.VMNic{
		ID: deletedID, VmID: uuid.New(), NetworkID: netID, DeviceOrder: 2,
		Model: store.NicModelVirtio, Generation: 1, CreatedAt: now, UpdatedAt: now,
		DeletedAt: &now,
	}); err != nil {
		t.Fatalf("seed soft-deleted nic: %v", err)
	}
	if err := s.c.Put(ctx, vmNicNetworkIndexKey(netID, deletedID), []byte(deletedID.String())); err != nil {
		t.Fatalf("seed index deleted: %v", err)
	}

	// Stale index entry whose NIC row is gone (hard-gone): skipped.
	staleID := uuid.New()
	if err := s.c.Put(ctx, vmNicNetworkIndexKey(netID, staleID), []byte(staleID.String())); err != nil {
		t.Fatalf("seed stale index: %v", err)
	}

	got, err := s.ListVMNicsByNetwork(ctx, netID)
	if err != nil {
		t.Fatalf("ListVMNicsByNetwork: %v", err)
	}

	gotIDs := make(map[uuid.UUID]bool, len(got))
	for _, n := range got {
		gotIDs[n.ID] = true
	}
	want := map[uuid.UUID]bool{withIP: true, noIP: true}
	if len(got) != len(want) {
		t.Fatalf("ListVMNicsByNetwork returned %d rows, want %d", len(got), len(want))
	}
	for id := range want {
		if !gotIDs[id] {
			t.Errorf("ListVMNicsByNetwork missing live NIC %s", id)
		}
	}
	if gotIDs[deletedID] {
		t.Errorf("ListVMNicsByNetwork returned soft-deleted NIC %s, want excluded", deletedID)
	}
	if gotIDs[staleID] {
		t.Errorf("ListVMNicsByNetwork returned stale-index NIC %s, want skipped", staleID)
	}

	// The Ipv4Address round-trips on the row that carries one.
	for _, n := range got {
		if n.ID == withIP {
			if n.Ipv4Address == nil || *n.Ipv4Address != ip {
				t.Errorf("withIP NIC Ipv4Address = %v, want %v", n.Ipv4Address, ip)
			}
		}
	}
}

// ptrPrefix parses a CIDR string and returns a pointer to the prefix, for
// seeding network Subnet fields in tests.
func ptrPrefix(cidr string) *netip.Prefix {
	p := netip.MustParsePrefix(cidr)
	return &p
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
