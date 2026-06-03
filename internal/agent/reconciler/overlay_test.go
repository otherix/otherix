// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"net/netip"
	"testing"
	"time"

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
