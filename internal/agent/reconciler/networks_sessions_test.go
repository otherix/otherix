// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
)

// TestNetworksSessionCounterFoldsIntoReport confirms the per-network live-session
// counter the connect plane maintains lands on the matching NetworkReport, keyed
// by the same network id the report uses, and that acquire/release balance.
func TestNetworksSessionCounterFoldsIntoReport(t *testing.T) {
	rec, err := NewNetworks(&netfabric.FakeFabric{}, nil, discardLogger(), time.Minute, false)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
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

// TestOverlayCandidatesForIP confirms the resolver returns the overlay veth
// host-end device and the network id whose declared subnet contains the IP for an
// overlay this node holds a gateway membership on, so the connect plane can source
// its dial from the veth and key its per-network session counter the same way the
// report does. A non-overlapping IP yields exactly one candidate; an off-overlay
// IP yields none.
func TestOverlayCandidatesForIP(t *testing.T) {
	rec, err := NewNetworks(&netfabric.FakeFabric{}, nil, discardLogger(), time.Minute, false)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	netID := "22222222-2222-2222-2222-222222222222"
	subnet := "10.50.0.0/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{
				ID: netID, Type: "overlay", BridgeName: "otvb200", Mtu: 1450, VNI: i32(200), Subnet: &subnet,
				GatewayAddr: &heartbeat.GatewayAddr{IP: "10.50.0.1", MAC: "02:00:0a:32:00:01"},
			},
		},
	})

	// A gateway overlay resolves to its veth host end, not the bridge.
	got := rec.OverlayCandidatesForIP(netip.MustParseAddr("10.50.0.7"))
	want := []OverlayCandidate{{Device: "otvg200", NetworkID: netID}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("OverlayCandidatesForIP mismatch (-want +got):\n%s", diff)
	}
	// An off-overlay IP resolves to nothing.
	if cands := rec.OverlayCandidatesForIP(netip.MustParseAddr("192.168.0.1")); len(cands) != 0 {
		t.Fatalf("OverlayCandidatesForIP for an off-overlay IP = %+v, want empty", cands)
	}
}

// TestOverlayCandidatesRequireMembership proves the resolver fails closed: an
// overlay whose declared subnet contains the IP but on which this node holds no
// gateway membership (GatewayAddr nil) must not appear as a candidate, because
// there is no veth to source the dial from.
func TestOverlayCandidatesRequireMembership(t *testing.T) {
	rec, err := NewNetworks(&netfabric.FakeFabric{}, nil, discardLogger(), time.Minute, false)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{{
			ID: "net-x", Type: "overlay", BridgeName: "otvb300",
			VNI: i32(300), Subnet: strptr("10.60.0.0/16"), Mtu: 1390,
			GatewayAddr: nil,
		}},
	})
	if cands := rec.OverlayCandidatesForIP(netip.MustParseAddr("10.60.0.5")); len(cands) != 0 {
		t.Fatalf("OverlayCandidatesForIP on a non-membership overlay = %+v, want empty (fail closed)", cands)
	}
}

// TestOverlayResolverOverlappingSubnets proves the resolver enumerates EVERY
// gateway overlay whose subnet contains the IP when two overlays share the same
// subnet (a legal per-tenant configuration), independent of declared slice order,
// so the caller can disambiguate by neighbor MAC. The pre-fix single-return
// resolver could only ever surface the first-declared overlay, refusing a
// legitimate session to the second.
func TestOverlayResolverOverlappingSubnets(t *testing.T) {
	rec, err := NewNetworks(&netfabric.FakeFabric{}, nil, discardLogger(), time.Minute, false)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	subnet := "10.0.0.0/16"
	netA := heartbeat.DeclaredNetwork{
		ID: "net-A", Type: "overlay", BridgeName: "otvb100", Mtu: 1450, VNI: i32(100), Subnet: &subnet,
		GatewayAddr: &heartbeat.GatewayAddr{IP: "10.0.0.1", MAC: "02:00:00:00:01:00"},
	}
	netB := heartbeat.DeclaredNetwork{
		ID: "net-B", Type: "overlay", BridgeName: "otvb200", Mtu: 1450, VNI: i32(200), Subnet: &subnet,
		GatewayAddr: &heartbeat.GatewayAddr{IP: "10.0.0.2", MAC: "02:00:00:00:02:00"},
	}

	// Both declared orders must yield both candidates.
	orders := map[string][]heartbeat.DeclaredNetwork{
		"A_before_B": {netA, netB},
		"B_before_A": {netB, netA},
	}
	want := map[string]string{"otvg100": "net-A", "otvg200": "net-B"}
	for name, order := range orders {
		t.Run(name, func(t *testing.T) {
			rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{DeclaredNetworks: order})
			got := map[string]string{}
			for _, c := range rec.OverlayCandidatesForIP(netip.MustParseAddr("10.0.0.5")) {
				got[c.Device] = c.NetworkID
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("candidates mismatch (-want +got):\n%s", diff)
			}
		})
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
