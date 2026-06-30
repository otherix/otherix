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

// TestNetworksSessionCounterFoldsIntoReport confirms the per-network live-session
// counter the connect plane maintains lands on the matching NetworkReport, keyed
// by the same network id the report uses, and that acquire/release balance.
func TestNetworksSessionCounterFoldsIntoReport(t *testing.T) {
	rec, err := NewGatewayNetworks(&netfabric.FakeFabric{}, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewGatewayNetworks: %v", err)
	}
	netID := "11111111-1111-1111-1111-111111111111"
	subnet := "10.42.0.0/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: netID, Name: "ov", Type: "overlay", BridgeName: "otvb100", Mtu: 1450, VNI: i32(100), Subnet: &subnet},
		},
	})
	rec.reconcile(context.Background())

	// Two sessions acquired, one released: the report must show one live session.
	rec.AcquireSession(netID)
	rec.AcquireSession(netID)
	rec.ReleaseSession(netID)

	rep := reportFor(t, rec, netID)
	if rep.ActiveSessions != 1 {
		t.Fatalf("ActiveSessions = %d, want 1 (2 acquired, 1 released)", rep.ActiveSessions)
	}

	// Releasing the last session returns the count to zero.
	rec.ReleaseSession(netID)
	rep = reportFor(t, rec, netID)
	if rep.ActiveSessions != 0 {
		t.Fatalf("ActiveSessions = %d, want 0 after balanced release", rep.ActiveSessions)
	}

	// A release with no outstanding session must not drive the count negative.
	rec.ReleaseSession(netID)
	rep = reportFor(t, rec, netID)
	if rep.ActiveSessions != 0 {
		t.Fatalf("ActiveSessions = %d, want 0 (release must not underflow)", rep.ActiveSessions)
	}
}

// TestNetworksOverlayNetworkForIP confirms the resolver returns both the overlay
// bridge and the network id whose declared subnet contains the IP, so the
// connect plane can key its per-network session counter the same way the report
// does.
func TestNetworksOverlayNetworkForIP(t *testing.T) {
	rec, err := NewGatewayNetworks(&netfabric.FakeFabric{}, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewGatewayNetworks: %v", err)
	}
	netID := "22222222-2222-2222-2222-222222222222"
	subnet := "10.50.0.0/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: netID, Type: "overlay", BridgeName: "otvb200", Mtu: 1450, VNI: i32(200), Subnet: &subnet},
		},
	})

	bridge, gotID, ok := rec.OverlayNetworkForIP(netip.MustParseAddr("10.50.0.7"))
	if !ok || bridge != "otvb200" || gotID != netID {
		t.Fatalf("OverlayNetworkForIP = (%q, %q, %v), want (otvb200, %s, true)", bridge, gotID, ok, netID)
	}
	if _, _, ok := rec.OverlayNetworkForIP(netip.MustParseAddr("192.168.0.1")); ok {
		t.Fatalf("OverlayNetworkForIP for an off-overlay IP = ok, want not ok")
	}
}

func i32(v int32) *int32 { return &v }

// reportFor returns the NetworkReport for id, failing the test when absent.
func reportFor(t *testing.T, rec *Networks, id string) heartbeat.NetworkReport {
	t.Helper()
	for _, r := range rec.NetworkReports() {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no NetworkReport for %s", id)
	return heartbeat.NetworkReport{}
}
