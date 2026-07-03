// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
)

// overlayEgressNet returns overlayNet() with egress=nat enabled.
func overlayEgressNet() heartbeat.DeclaredNetwork {
	d := overlayNet()
	d.Egress = "nat"
	return d
}

// readyEgressFabric is a fabric primed so applyOverlay reaches the egress
// branch: otwg0 up carrying the self overlay IP.
func readyEgressFabric() *netfabric.FakeFabric {
	return &netfabric.FakeFabric{LinkStateResult: map[string]netfabric.LinkState{
		"otwg0": {Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.42.0.5/16")}},
	}}
}

func TestApplyOverlayEgressInstallsGatewayAndMasq(t *testing.T) {
	f := readyEgressFabric()
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute, true)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayEgressNet()},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	var rep heartbeat.NetworkReport
	for _, r := range rec.NetworkReports() {
		if r.ID == "ov1" {
			rep = r
		}
	}
	if rep.ReconciliationStatus != "ready" {
		t.Fatalf("status = %q (%v), want ready", rep.ReconciliationStatus, rep.ReconciliationError)
	}
	if f.EnableIPForwardingCalls == 0 {
		t.Errorf("EnableIPForwarding not called for egress overlay")
	}
	if len(f.AnycastGatewayCalls) != 1 {
		t.Fatalf("AnycastGatewayCalls = %d, want 1", len(f.AnycastGatewayCalls))
	}
	gw := f.AnycastGatewayCalls[0]
	if gw.Bridge != "otvb1000" {
		t.Errorf("anycast gateway bridge = %q, want otvb1000", gw.Bridge)
	}
	if gw.Addr != netfabric.OverlayGatewayAddr {
		t.Errorf("anycast gateway addr = %v, want %v", gw.Addr, netfabric.OverlayGatewayAddr)
	}
	if want := netfabric.GatewayMAC(1000).String(); gw.MAC != want {
		t.Errorf("anycast gateway MAC = %q, want %q", gw.MAC, want)
	}
	if len(f.MasqueradeIfaceCalls) != 1 || f.MasqueradeIfaceCalls[0].InIface != "otvb1000" {
		t.Errorf("MasqueradeIfaceCalls = %+v, want one for otvb1000", f.MasqueradeIfaceCalls)
	}
	if !rec.applied["ov1"].HasEgress {
		t.Errorf("applied[ov1].HasEgress = false, want true")
	}
}

func TestApplyOverlayEgressInstallsSubnetRoute(t *testing.T) {
	f := readyEgressFabric()
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute, true)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	d := overlayEgressNet()
	subnet := "10.62.0.0/24"
	d.Subnet = &subnet
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{d},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	if len(f.BridgeRouteCalls) != 1 {
		t.Fatalf("BridgeRouteCalls = %d, want 1 (overlay subnet route for return traffic)", len(f.BridgeRouteCalls))
	}
	got := f.BridgeRouteCalls[0]
	if got.Bridge != "otvb1000" || got.Subnet.String() != "10.62.0.0/24" {
		t.Errorf("BridgeRouteCall = %+v, want {10.62.0.0/24 otvb1000}", got)
	}
}

func TestApplyOverlayNoEgressSkipsMasq(t *testing.T) {
	f := readyEgressFabric()
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute, true)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayNet()}, // egress=""
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	var rep heartbeat.NetworkReport
	for _, r := range rec.NetworkReports() {
		if r.ID == "ov1" {
			rep = r
		}
	}
	if rep.ReconciliationStatus != "ready" {
		t.Fatalf("status = %q, want ready", rep.ReconciliationStatus)
	}
	if f.EnableIPForwardingCalls != 0 {
		t.Errorf("EnableIPForwarding called for egress=none overlay")
	}
	if len(f.AnycastGatewayCalls) != 0 {
		t.Errorf("AnycastGateway called for egress=none overlay: %+v", f.AnycastGatewayCalls)
	}
	if len(f.MasqueradeIfaceCalls) != 0 {
		t.Errorf("MasqueradeIface called for egress=none overlay: %+v", f.MasqueradeIfaceCalls)
	}
	if rec.applied["ov1"].HasEgress {
		t.Errorf("HasEgress set for egress=none overlay")
	}
}

func TestApplyOverlayEgressErrorStaysReady(t *testing.T) {
	f := readyEgressFabric()
	f.Errs = map[string]error{"EnsureMasqueradeIface": errors.New("masq boom")}
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute, true)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayEgressNet()},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	var rep heartbeat.NetworkReport
	for _, r := range rec.NetworkReports() {
		if r.ID == "ov1" {
			rep = r
		}
	}
	// L2 (VTEP/bridge/FDB) is converged; an egress install failure must NOT
	// mark the whole overlay failed - that would wrongly de-schedule the node.
	if rep.ReconciliationStatus != "ready" {
		t.Errorf("status = %q, want ready (egress failure must not fail the network)", rep.ReconciliationStatus)
	}
	// Egress did not fully install, so HasEgress must be false (teardown/observability).
	if rec.applied["ov1"].HasEgress {
		t.Errorf("HasEgress = true, want false (egress install failed)")
	}
}

func TestApplyOverlayEgressTeardownRemovesMasq(t *testing.T) {
	f := readyEgressFabric()
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute, true)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayEgressNet()},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background()) // materialise
	// CP deletes the network: empty declared set.
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{SelfOverlayIP: &ip})
	rec.reconcile(context.Background()) // teardown
	if len(f.RemoveMasqIfaceCalls) != 1 || f.RemoveMasqIfaceCalls[0] != "otvb1000" {
		t.Errorf("RemoveMasqIfaceCalls = %v, want [otvb1000]", f.RemoveMasqIfaceCalls)
	}
	if len(f.RemoveVXLANCalls) != 1 || len(f.RemoveBridgeCalls) != 1 {
		t.Errorf("overlay not fully torn down: vxlan=%v bridge=%v", f.RemoveVXLANCalls, f.RemoveBridgeCalls)
	}
}
