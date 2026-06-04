// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
)

func vni(n int32) *int32 { return &n }

func overlayNet() heartbeat.DeclaredNetwork {
	return heartbeat.DeclaredNetwork{
		ID: "ov1", Name: "ov", Type: "overlay", Managed: true,
		BridgeName: "otb1000", Mtu: 1390, VNI: vni(1000),
	}
}

// drive runs one reconcile pass with the given overlay IP + fabric, and returns
// the report for id "ov1".
func drive(t *testing.T, f netfabric.Fabric, selfIP string) heartbeat.NetworkReport {
	t.Helper()
	rec, err := NewNetworks(f, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	var ipPtr *string
	if selfIP != "" {
		ipPtr = &selfIP
	}
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayNet()},
		SelfOverlayIP:    ipPtr,
	})
	rec.reconcile(context.Background())
	for _, rep := range rec.NetworkReports() {
		if rep.ID == "ov1" {
			return rep
		}
	}
	t.Fatalf("no report for ov1")
	return heartbeat.NetworkReport{}
}

func TestApplyOverlayPendingWhenNoOverlayIP(t *testing.T) {
	f := &netfabric.FakeFabric{}
	rep := drive(t, f, "")
	if rep.ReconciliationStatus != "pending" {
		t.Errorf("status = %q, want pending", rep.ReconciliationStatus)
	}
	if len(f.EnsureVXLANCalls) != 0 || len(f.EnsureBridgeCalls) != 0 {
		t.Errorf("fabric mutated while pending: vxlan=%d bridge=%d", len(f.EnsureVXLANCalls), len(f.EnsureBridgeCalls))
	}
}

func TestApplyOverlayPendingWhenOtwg0Down(t *testing.T) {
	f := &netfabric.FakeFabric{LinkStateResult: map[string]netfabric.LinkState{"otwg0": {Up: false}}}
	rep := drive(t, f, "10.42.0.5/16")
	if rep.ReconciliationStatus != "pending" {
		t.Errorf("status = %q, want pending", rep.ReconciliationStatus)
	}
	if len(f.EnsureVXLANCalls) != 0 {
		t.Errorf("VTEP created while otwg0 down")
	}
}

func TestApplyOverlayPendingWhenAddrMismatch(t *testing.T) {
	f := &netfabric.FakeFabric{LinkStateResult: map[string]netfabric.LinkState{
		"otwg0": {Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.42.0.9/16")}},
	}}
	rep := drive(t, f, "10.42.0.5/16")
	if rep.ReconciliationStatus != "pending" {
		t.Errorf("status = %q, want pending (otwg0 carries wrong addr)", rep.ReconciliationStatus)
	}
	if len(f.EnsureVXLANCalls) != 0 {
		t.Errorf("VTEP created while otwg0 carries the wrong address")
	}
}

func TestApplyOverlayReadyMaterializes(t *testing.T) {
	f := &netfabric.FakeFabric{LinkStateResult: map[string]netfabric.LinkState{
		"otwg0": {Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.42.0.5/16")}},
	}}
	rep := drive(t, f, "10.42.0.5/16")
	if rep.ReconciliationStatus != "ready" {
		t.Fatalf("status = %q, want ready", rep.ReconciliationStatus)
	}
	if len(f.EnsureBridgeCalls) != 1 || f.EnsureBridgeCalls[0] != (netfabric.BridgeCall{Name: "otb1000", MTU: 1390}) {
		t.Errorf("EnsureBridgeCalls = %+v, want [{otb1000 1390}]", f.EnsureBridgeCalls)
	}
	if len(f.EnsureVXLANCalls) != 1 {
		t.Fatalf("EnsureVXLANCalls = %d, want 1", len(f.EnsureVXLANCalls))
	}
	got := f.EnsureVXLANCalls[0]
	want := netfabric.VXLANConfig{VNI: 1000, Local: netip.MustParseAddr("10.42.0.5"), Port: 4789, MTU: 1390, Master: "otb1000"}
	if got != want {
		t.Errorf("EnsureVXLAN cfg = %+v, want %+v", got, want)
	}
}

func TestApplyOverlayTeardownOnUndeclare(t *testing.T) {
	f := &netfabric.FakeFabric{LinkStateResult: map[string]netfabric.LinkState{
		"otwg0": {Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.42.0.5/16")}},
	}}
	rec, err := NewNetworks(f, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayNet()},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())
	// Undeclare: empty declared set.
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{SelfOverlayIP: &ip})
	rec.reconcile(context.Background())

	if len(f.RemoveVXLANCalls) != 1 || f.RemoveVXLANCalls[0] != 1000 {
		t.Errorf("RemoveVXLANCalls = %v, want [1000]", f.RemoveVXLANCalls)
	}
	if len(f.RemoveBridgeCalls) != 1 || f.RemoveBridgeCalls[0] != "otb1000" {
		t.Errorf("RemoveBridgeCalls = %v, want [otb1000]", f.RemoveBridgeCalls)
	}
}

// TestApplyOverlayTeardownAfterEnsureVXLANFailed is the bridge-leak-asymmetry
// guarantee, mirroring applyManaged's EnsureBridge-ok-but-NAT-failed case: when
// EnsureBridge SUCCEEDS but EnsureVXLAN persistently FAILS, the network must still
// be recorded in r.applied so a later CP-side delete (while the VTEP is still
// failing) tears the orphaned otb<vni> bridge down. Without the early record the
// bridge orphans on the host with no GC (r.applied is in-process only).
func TestApplyOverlayTeardownAfterEnsureVXLANFailed(t *testing.T) {
	f := &netfabric.FakeFabric{
		LinkStateResult: map[string]netfabric.LinkState{
			"otwg0": {Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.42.0.5/16")}},
		},
		Errs: map[string]error{"EnsureVXLAN": errors.New("vxlan boom")},
	}
	rec, err := NewNetworks(f, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayNet()},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	// EnsureBridge ran, EnsureVXLAN was attempted and failed: report is failed.
	var rep heartbeat.NetworkReport
	for _, r := range rec.NetworkReports() {
		if r.ID == "ov1" {
			rep = r
		}
	}
	if rep.ReconciliationStatus != "failed" {
		t.Errorf("status = %q, want failed (EnsureVXLAN error)", rep.ReconciliationStatus)
	}
	if len(f.EnsureBridgeCalls) != 1 {
		t.Errorf("EnsureBridgeCalls = %d, want 1", len(f.EnsureBridgeCalls))
	}
	// The network must be recorded despite the VXLAN failure, with the fields a
	// teardown needs (bridge name + VNI).
	a, ok := rec.applied["ov1"]
	if !ok {
		t.Fatalf("ov1 absent from r.applied after EnsureBridge-ok/EnsureVXLAN-failed; bridge would orphan on a later CP delete")
	}
	if a.BridgeName != "otb1000" || !a.Overlay || a.VNI != 1000 {
		t.Errorf("applied[ov1] = %+v, want {BridgeName:otb1000 Overlay:true VNI:1000}", a)
	}

	// Now undeclare the network (VXLAN still failing). teardownManaged must run
	// for it: RemoveVXLAN + RemoveBridge, and the id leaves r.applied.
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{SelfOverlayIP: &ip})
	rec.reconcile(context.Background())

	if len(f.RemoveBridgeCalls) != 1 || f.RemoveBridgeCalls[0] != "otb1000" {
		t.Errorf("RemoveBridgeCalls = %v, want [otb1000] (orphaned bridge torn down on undeclare)", f.RemoveBridgeCalls)
	}
	if len(f.RemoveVXLANCalls) != 1 || f.RemoveVXLANCalls[0] != 1000 {
		t.Errorf("RemoveVXLANCalls = %v, want [1000]", f.RemoveVXLANCalls)
	}
	if _, ok := rec.applied["ov1"]; ok {
		t.Errorf("ov1 still in r.applied after a clean teardown")
	}
}

func TestApplyOverlayFailedOnInvalidVNI(t *testing.T) {
	f := &netfabric.FakeFabric{LinkStateResult: map[string]netfabric.LinkState{
		"otwg0": {Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.42.0.5/16")}},
	}}
	rec, err := NewNetworks(f, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	bad := overlayNet()
	bad.VNI = vni(0) // invalid: VNI must be > 0
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{bad},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	var rep heartbeat.NetworkReport
	for _, r := range rec.NetworkReports() {
		if r.ID == "ov1" {
			rep = r
		}
	}
	if rep.ReconciliationStatus != "failed" {
		t.Errorf("status = %q, want failed (invalid vni)", rep.ReconciliationStatus)
	}
	if len(f.EnsureBridgeCalls) != 0 || len(f.EnsureVXLANCalls) != 0 {
		t.Errorf("fabric mutated on invalid vni: bridge=%d vxlan=%d", len(f.EnsureBridgeCalls), len(f.EnsureVXLANCalls))
	}
}

func TestApplyOverlayFailedOnFabricError(t *testing.T) {
	f := &netfabric.FakeFabric{
		LinkStateResult: map[string]netfabric.LinkState{
			"otwg0": {Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.42.0.5/16")}},
		},
		Errs: map[string]error{"EnsureBridge": errors.New("bridge boom")},
	}
	rep := drive(t, f, "10.42.0.5/16")
	if rep.ReconciliationStatus != "failed" {
		t.Errorf("status = %q, want failed (EnsureBridge error)", rep.ReconciliationStatus)
	}
	// The bridge was attempted; the VTEP must NOT be, since EnsureBridge failed first.
	if len(f.EnsureVXLANCalls) != 0 {
		t.Errorf("VTEP created after EnsureBridge failed")
	}
}

func readyFabricWithFDB(current []netfabric.FDBEntry) *netfabric.FakeFabric {
	return &netfabric.FakeFabric{
		LinkStateResult: map[string]netfabric.LinkState{
			"otwg0": {Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.42.0.5/16")}},
		},
		FDBListResult: current,
	}
}

func driveFDB(t *testing.T, f *netfabric.FakeFabric, fdb []heartbeat.DeclaredFDBEntry) {
	t.Helper()
	rec, err := NewNetworks(f, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayNet()},
		SelfOverlayIP:    &ip,
		DeclaredFDB:      fdb,
	})
	rec.reconcile(context.Background())
}

func TestApplyOverlayProgramsFDB(t *testing.T) {
	f := readyFabricWithFDB(nil)
	driveFDB(t, f, []heartbeat.DeclaredFDBEntry{
		{VNI: 1000, MAC: "52:54:00:00:00:0b", VtepIP: "10.42.0.2"},
		{VNI: 1000, MAC: "00:00:00:00:00:00", VtepIP: "10.42.0.2"},
	})
	if len(f.FDBAppendCalls) != 2 {
		t.Fatalf("FDBAppendCalls = %d, want 2: %+v", len(f.FDBAppendCalls), f.FDBAppendCalls)
	}
	for _, c := range f.FDBAppendCalls {
		if c.VNI != 1000 {
			t.Errorf("append VNI = %d, want 1000", c.VNI)
		}
		if c.Entry.Dst != netip.MustParseAddr("10.42.0.2") {
			t.Errorf("append dst = %v, want 10.42.0.2", c.Entry.Dst)
		}
	}
}

func TestApplyOverlayPrunesStaleFDB(t *testing.T) {
	stale := netfabric.FDBEntry{MAC: mustMAC("52:54:00:00:00:ff"), Dst: netip.MustParseAddr("10.42.0.9")}
	f := readyFabricWithFDB([]netfabric.FDBEntry{stale})
	driveFDB(t, f, nil)
	if len(f.FDBDeleteCalls) != 1 || f.FDBDeleteCalls[0].Entry.MAC.String() != "52:54:00:00:00:ff" {
		t.Errorf("FDBDeleteCalls = %+v, want one delete of 52:54:00:00:00:ff", f.FDBDeleteCalls)
	}
	if len(f.FDBAppendCalls) != 0 {
		t.Errorf("unexpected appends: %+v", f.FDBAppendCalls)
	}
}

// TestApplyOverlayMovedMACPrunesBeforeAppending is the dual-homing guarantee:
// a remote VM's MAC moves from vtepA to vtepB (re-placement). Keyed on (mac,dst)
// this is a delete-old + add-new, and the prune MUST run before the append so the
// kernel FDB never transiently holds both (mac,vtepA) and (mac,vtepB) - under
// VXLAN nolearning the driver would replicate the unicast to both dsts (duplicate
// delivery + blackhole on the old node). A brief no-entry window (a recoverable
// drop) is strictly less harmful than dual-dst delivery and reconverges same pass.
func TestApplyOverlayMovedMACPrunesBeforeAppending(t *testing.T) {
	const mac = "52:54:00:00:00:0b"
	// Current FDB has the MAC pointing at the OLD vtep (10.42.0.9).
	old := netfabric.FDBEntry{MAC: mustMAC(mac), Dst: netip.MustParseAddr("10.42.0.9")}
	f := readyFabricWithFDB([]netfabric.FDBEntry{old})
	// Declared moves the same MAC to the NEW vtep (10.42.0.2).
	driveFDB(t, f, []heartbeat.DeclaredFDBEntry{{VNI: 1000, MAC: mac, VtepIP: "10.42.0.2"}})

	if len(f.FDBDeleteCalls) != 1 || f.FDBDeleteCalls[0].Entry.Dst != netip.MustParseAddr("10.42.0.9") {
		t.Errorf("FDBDeleteCalls = %+v, want one delete of dst 10.42.0.9", f.FDBDeleteCalls)
	}
	if len(f.FDBAppendCalls) != 1 || f.FDBAppendCalls[0].Entry.Dst != netip.MustParseAddr("10.42.0.2") {
		t.Errorf("FDBAppendCalls = %+v, want one append of dst 10.42.0.2", f.FDBAppendCalls)
	}
	// The delete (prune of the old dst) must come before the append (the new dst):
	// at no point may both entries coexist in the kernel FDB.
	want := []string{"delete", "append"}
	if len(f.FDBOpLog) != 2 || f.FDBOpLog[0] != want[0] || f.FDBOpLog[1] != want[1] {
		t.Errorf("FDBOpLog = %v, want %v (prune must precede append for a moved mac)", f.FDBOpLog, want)
	}
}

// TestApplyOverlayFDBSkipsAppendWhenPruneDeleteFails is the F1 fail-closed
// guarantee: a MAC moves vtepA -> vtepB, so the diff is delete (mac,vtepA) +
// append (mac,vtepB). If the prune-delete transiently FAILS, the append MUST be
// skipped this pass - otherwise, under nolearning, the kernel FDB transiently
// holds the MAC on BOTH next-hops (duplicate delivery + wrong-node flood). The
// next pass retries the delete first; the overlay stays pending meanwhile.
func TestApplyOverlayFDBSkipsAppendWhenPruneDeleteFails(t *testing.T) {
	const mac = "52:54:00:00:00:0b"
	old := netfabric.FDBEntry{MAC: mustMAC(mac), Dst: netip.MustParseAddr("10.42.0.9")}
	f := readyFabricWithFDB([]netfabric.FDBEntry{old})
	f.Errs = map[string]error{"FDBDelete": errors.New("delete boom")}
	rep := driveFDBReport(t, f, []heartbeat.DeclaredFDBEntry{{VNI: 1000, MAC: mac, VtepIP: "10.42.0.2"}})

	// The prune-delete was attempted (and failed).
	if len(f.FDBDeleteCalls) != 1 || f.FDBDeleteCalls[0].Entry.Dst != netip.MustParseAddr("10.42.0.9") {
		t.Errorf("FDBDeleteCalls = %+v, want one delete of dst 10.42.0.9", f.FDBDeleteCalls)
	}
	// The new dst was NOT programmed: appending it while the stale entry lingers
	// would dual-home the MAC.
	if len(f.FDBAppendCalls) != 0 {
		t.Errorf("FDBAppendCalls = %+v, want none (append skipped because prune-delete failed)", f.FDBAppendCalls)
	}
	// No append op for this MAC appears in the op log.
	for _, op := range f.FDBOpLogMAC {
		if op == "append:"+mac {
			t.Errorf("FDBOpLogMAC = %v, contains an append for the moved mac despite the failed prune", f.FDBOpLogMAC)
		}
	}
	// The overlay holds pending; the FDB did not converge.
	if rep.ReconciliationStatus != "pending" {
		t.Errorf("status = %q, want pending (fdb not converged after failed prune)", rep.ReconciliationStatus)
	}
}

// TestApplyOverlayFDBAppendsAfterSuccessfulPrune is the revert-to-confirm twin of
// the F1 guard: the SAME move, but the prune-delete SUCCEEDS, so the new dst IS
// programmed - op order delete(mac) THEN append(mac). This proves the append skip
// is conditional on the delete FAILING, not an unconditional suppression.
func TestApplyOverlayFDBAppendsAfterSuccessfulPrune(t *testing.T) {
	const mac = "52:54:00:00:00:0b"
	old := netfabric.FDBEntry{MAC: mustMAC(mac), Dst: netip.MustParseAddr("10.42.0.9")}
	f := readyFabricWithFDB([]netfabric.FDBEntry{old})
	driveFDB(t, f, []heartbeat.DeclaredFDBEntry{{VNI: 1000, MAC: mac, VtepIP: "10.42.0.2"}})

	if len(f.FDBDeleteCalls) != 1 || f.FDBDeleteCalls[0].Entry.Dst != netip.MustParseAddr("10.42.0.9") {
		t.Errorf("FDBDeleteCalls = %+v, want one delete of dst 10.42.0.9", f.FDBDeleteCalls)
	}
	if len(f.FDBAppendCalls) != 1 || f.FDBAppendCalls[0].Entry.Dst != netip.MustParseAddr("10.42.0.2") {
		t.Errorf("FDBAppendCalls = %+v, want one append of dst 10.42.0.2", f.FDBAppendCalls)
	}
	want := []string{"delete:" + mac, "append:" + mac}
	if diff := cmp.Diff(want, f.FDBOpLogMAC); diff != "" {
		t.Errorf("FDBOpLogMAC mismatch (-want +got):\n%s", diff)
	}
}

func TestApplyOverlayFDBIdempotentSteadyState(t *testing.T) {
	entry := netfabric.FDBEntry{MAC: mustMAC("52:54:00:00:00:0b"), Dst: netip.MustParseAddr("10.42.0.2")}
	f := readyFabricWithFDB([]netfabric.FDBEntry{entry})
	driveFDB(t, f, []heartbeat.DeclaredFDBEntry{{VNI: 1000, MAC: "52:54:00:00:00:0b", VtepIP: "10.42.0.2"}})
	if len(f.FDBAppendCalls) != 0 || len(f.FDBDeleteCalls) != 0 {
		t.Errorf("steady state mutated FDB: appends=%+v deletes=%+v", f.FDBAppendCalls, f.FDBDeleteCalls)
	}
}

func TestApplyOverlaySkipsUnparseableFDB(t *testing.T) {
	f := readyFabricWithFDB(nil)
	rep := driveFDBReport(t, f, []heartbeat.DeclaredFDBEntry{
		{VNI: 1000, MAC: "not-a-mac", VtepIP: "10.42.0.2"},
		{VNI: 1000, MAC: "52:54:00:00:00:0b", VtepIP: "bogus-ip"},
		{VNI: 1000, MAC: "52:54:00:00:00:0c", VtepIP: "10.42.0.3"},
	})
	// The good entry is still programmed: one bad entry never drops the rest.
	if len(f.FDBAppendCalls) != 1 || f.FDBAppendCalls[0].Entry.MAC.String() != "52:54:00:00:00:0c" {
		t.Errorf("want only the valid entry appended, got %+v", f.FDBAppendCalls)
	}
	// Two declared entries could not be parsed; the overlay must hold pending
	// with a reason naming the unparseable count, not falsely report ready over a
	// per-VM unicast blackhole.
	if rep.ReconciliationStatus != "pending" {
		t.Errorf("status = %q, want pending (unparseable fdb entries)", rep.ReconciliationStatus)
	}
	if rep.ReconciliationError == nil || *rep.ReconciliationError != "fdb_unparseable_entries=2" {
		got := "<nil>"
		if rep.ReconciliationError != nil {
			got = *rep.ReconciliationError
		}
		t.Errorf("reason = %q, want %q", got, "fdb_unparseable_entries=2")
	}
}

// driveFDBReport runs one reconcile pass with the given declared FDB over a
// VTEP-ready fabric and returns the report for id "ov1" (overlayNet()'s id).
func driveFDBReport(t *testing.T, f *netfabric.FakeFabric, fdb []heartbeat.DeclaredFDBEntry) heartbeat.NetworkReport {
	t.Helper()
	rec, err := NewNetworks(f, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayNet()},
		SelfOverlayIP:    &ip,
		DeclaredFDB:      fdb,
	})
	rec.reconcile(context.Background())
	for _, rep := range rec.NetworkReports() {
		if rep.ID == "ov1" {
			return rep
		}
	}
	t.Fatalf("no report for ov1")
	return heartbeat.NetworkReport{}
}

func TestApplyOverlayPendingWhenFDBListFails(t *testing.T) {
	f := readyFabricWithFDB(nil)
	f.Errs = map[string]error{"FDBList": errors.New("netlink busy")}
	rep := driveFDBReport(t, f, []heartbeat.DeclaredFDBEntry{{VNI: 1000, MAC: "52:54:00:00:00:0b", VtepIP: "10.42.0.2"}})
	if rep.ReconciliationStatus != "pending" {
		t.Errorf("status = %q, want pending (fdb not converged)", rep.ReconciliationStatus)
	}
	if len(f.EnsureVXLANCalls) != 1 {
		t.Errorf("EnsureVXLAN calls = %d, want 1 (VTEP still materialised)", len(f.EnsureVXLANCalls))
	}
}

func TestApplyOverlayPendingWhenFDBAppendFails(t *testing.T) {
	f := readyFabricWithFDB(nil)
	f.Errs = map[string]error{"FDBAppend": errors.New("append boom")}
	rep := driveFDBReport(t, f, []heartbeat.DeclaredFDBEntry{{VNI: 1000, MAC: "52:54:00:00:00:0b", VtepIP: "10.42.0.2"}})
	if rep.ReconciliationStatus != "pending" {
		t.Errorf("status = %q, want pending (fdb append failed)", rep.ReconciliationStatus)
	}
}

func TestApplyOverlayReadyWhenFDBConverges(t *testing.T) {
	f := readyFabricWithFDB(nil)
	rep := driveFDBReport(t, f, []heartbeat.DeclaredFDBEntry{{VNI: 1000, MAC: "52:54:00:00:00:0b", VtepIP: "10.42.0.2"}})
	if rep.ReconciliationStatus != "ready" {
		t.Errorf("status = %q, want ready", rep.ReconciliationStatus)
	}
}

// TestApplyOverlayReadyDespiteSkippedNoIP is the non-blocking-signal guarantee:
// the CP omitted an unreachable remote peer's FDB entry (its node has no overlay
// IP yet) and surfaced that as overlay_reachability. The agent converges on the
// smaller programmable set it actually received and reports ready - a dead/IP-less
// peer must NEVER wedge the local overlay at pending. The reachability signal is
// observability only and does not feed the ready/pending decision.
func TestApplyOverlayReadyDespiteSkippedNoIP(t *testing.T) {
	f := readyFabricWithFDB(nil)
	rec, err := NewNetworks(f, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayNet()},
		SelfOverlayIP:    &ip,
		// Only the reachable peer is in declared_fdb; an unreachable peer was
		// dropped CP-side and surfaced here as a reachability shortfall.
		DeclaredFDB:         []heartbeat.DeclaredFDBEntry{{VNI: 1000, MAC: "52:54:00:00:00:0b", VtepIP: "10.42.0.2"}},
		OverlayReachability: []heartbeat.OverlayReachability{{VNI: 1000, SkippedNoIP: 1}},
	})
	rec.reconcile(context.Background())

	var rep heartbeat.NetworkReport
	for _, r := range rec.NetworkReports() {
		if r.ID == "ov1" {
			rep = r
		}
	}
	if rep.ReconciliationStatus != "ready" {
		t.Errorf("status = %q, want ready (per-peer skip is non-blocking)", rep.ReconciliationStatus)
	}
}

// TestApplyOverlayReadyDespiteUnestablishedFloodTargets is the C1-full
// readiness-unchanged guarantee: the CP surfaced that this VNI's flood targets
// have overlay IPs (so their FDB entries ARE programmed) but 0 of them have an
// established WireGuard handshake (established_peers=0 of total_peers=N). That is a
// blackhole-risk signal, but it must NEVER hold the local overlay at pending -
// gating on it would reintroduce the C4 wedge and flap on every WG rekey. The
// overlay converges on the FDB it received and reports ready; the established/total
// counts ride on the response as observability only.
func TestApplyOverlayReadyDespiteUnestablishedFloodTargets(t *testing.T) {
	f := readyFabricWithFDB(nil)
	rec, err := NewNetworks(f, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayNet()},
		SelfOverlayIP:    &ip,
		// Two flood targets, both programmed in the FDB, but neither has an
		// established tunnel: established_peers=0 of total_peers=2.
		DeclaredFDB: []heartbeat.DeclaredFDBEntry{
			{VNI: 1000, MAC: "52:54:00:00:00:0b", VtepIP: "10.42.0.2"},
			{VNI: 1000, MAC: "52:54:00:00:00:0c", VtepIP: "10.42.0.3"},
		},
		OverlayReachability: []heartbeat.OverlayReachability{
			{VNI: 1000, SkippedNoIP: 0, EstablishedPeers: 0, TotalPeers: 2},
		},
	})
	rec.reconcile(context.Background())

	var rep heartbeat.NetworkReport
	for _, r := range rec.NetworkReports() {
		if r.ID == "ov1" {
			rep = r
		}
	}
	if rep.ReconciliationStatus != "ready" {
		t.Errorf("status = %q, want ready (unestablished flood targets are non-blocking)", rep.ReconciliationStatus)
	}
}

func TestApplyOverlayFailsOnOversizeVNI(t *testing.T) {
	f := readyFabricWithFDB(nil)
	rec, err := NewNetworks(f, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	d := overlayNet()
	big := int32(0xFFFFFF + 1) // 16777216, one past the 24-bit ceiling
	d.VNI = &big
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{d},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())
	var rep heartbeat.NetworkReport
	for _, r := range rec.NetworkReports() {
		if r.ID == d.ID {
			rep = r
		}
	}
	if rep.ReconciliationStatus != "failed" {
		t.Errorf("status = %q, want failed (VNI over 24-bit ceiling)", rep.ReconciliationStatus)
	}
	if len(f.EnsureVXLANCalls) != 0 {
		t.Errorf("VTEP created for an out-of-range VNI")
	}
}

func TestApplyOverlayFailedOnLinkStateError(t *testing.T) {
	f := &netfabric.FakeFabric{
		LinkStateResult: map[string]netfabric.LinkState{
			"otwg0": {Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.42.0.5/16")}},
		},
		Errs: map[string]error{"LinkState": errors.New("netlink down")},
	}
	rep := drive(t, f, "10.42.0.5/16")
	if rep.ReconciliationStatus != "failed" {
		t.Errorf("status = %q, want failed (LinkState error)", rep.ReconciliationStatus)
	}
	if len(f.EnsureBridgeCalls) != 0 || len(f.EnsureVXLANCalls) != 0 {
		t.Errorf("fabric mutated after LinkState error: bridge=%d vxlan=%d", len(f.EnsureBridgeCalls), len(f.EnsureVXLANCalls))
	}
}

func TestApplyOverlayFailedOnUnparseableSelfIP(t *testing.T) {
	f := &netfabric.FakeFabric{
		LinkStateResult: map[string]netfabric.LinkState{
			"otwg0": {Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.42.0.5/16")}},
		},
	}
	rep := drive(t, f, "not-a-prefix")
	if rep.ReconciliationStatus != "failed" {
		t.Errorf("status = %q, want failed (unparseable self_overlay_ip)", rep.ReconciliationStatus)
	}
	if len(f.EnsureVXLANCalls) != 0 {
		t.Errorf("VTEP created for an unparseable self ip")
	}
}

func mustMAC(s string) net.HardwareAddr {
	m, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return m
}
