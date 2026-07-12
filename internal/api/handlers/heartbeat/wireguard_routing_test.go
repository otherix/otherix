// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"fmt"
	"net/netip"
	"testing"

	"github.com/google/uuid"
)

// id maps a small integer to a deterministic UUID whose ordering matches the
// integer (id(2) < id(3)), so the lowest-UUID relay selection is testable.
func id(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", n))
}

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

// findPeer returns the emitted entry with the given public key, or nil.
func findPeer(out []declaredWireGuardPeer, pubkey string) *declaredWireGuardPeer {
	for i := range out {
		if out[i].PublicKey == pubkey {
			return &out[i]
		}
	}
	return nil
}

// hasAllowed reports whether the entry's AllowedIPs contains the CIDR.
func hasAllowed(e *declaredWireGuardPeer, cidr string) bool {
	for _, a := range e.AllowedIPs {
		if a == cidr {
			return true
		}
	}
	return false
}

func TestRouting_DirectPeer(t *testing.T) {
	self := RoutingNode{NodeID: id(1), OverlayIP: ip("10.0.0.1")} // NAT'd
	pub := RoutingNode{NodeID: id(2), PublicKey: "p2", OverlayIP: ip("10.0.0.2"), Endpoint: "5.5.5.5:51820"}
	out := ComputeWireGuardRouting(self, []RoutingNode{pub})
	if len(out) != 1 || out[0].Endpoint != "5.5.5.5:51820" || out[0].AllowedIPs[0] != "10.0.0.2/32" {
		t.Fatalf("direct wrong: %+v", out)
	}
}

func TestRouting_GatewayForwardsToDialer(t *testing.T) {
	// self is a gateway; NAT'd peer A dialed it (in EstablishedPeers) -> direct entry, no endpoint
	self := RoutingNode{
		NodeID: id(2), OverlayIP: ip("10.0.0.2"), Endpoint: "2.2.2.2:51820", IsGateway: true,
		EstablishedPeers: map[uuid.UUID]bool{id(9): true},
	}
	a := RoutingNode{NodeID: id(9), PublicKey: "p9", OverlayIP: ip("10.0.0.9")} // NAT'd
	out := ComputeWireGuardRouting(self, []RoutingNode{a})
	e := findPeer(out, "p9")
	if e == nil || e.Endpoint != "" || !hasAllowed(e, "10.0.0.9/32") {
		t.Fatalf("gateway must forward to a dialer with an endpoint-less /32 entry: %+v", out)
	}
}

func TestRouting_RelayedMergeLowestUUID(t *testing.T) {
	self := RoutingNode{NodeID: id(9), OverlayIP: ip("10.0.0.9"), EstablishedPeers: map[uuid.UUID]bool{id(2): true, id(3): true}}
	peer := RoutingNode{NodeID: id(8), PublicKey: "p8", OverlayIP: ip("10.0.0.8"), EstablishedPeers: map[uuid.UUID]bool{id(2): true, id(3): true}}
	g1 := RoutingNode{NodeID: id(2), PublicKey: "g2", OverlayIP: ip("10.0.0.2"), Endpoint: "2.2.2.2:51820", IsGateway: true}
	g2 := RoutingNode{NodeID: id(3), PublicKey: "g3", OverlayIP: ip("10.0.0.3"), Endpoint: "3.3.3.3:51820", IsGateway: true}
	out := ComputeWireGuardRouting(self, []RoutingNode{peer, g1, g2})
	g := findPeer(out, "g2") // lowest UUID id(2)
	if g == nil || !hasAllowed(g, "10.0.0.2/32") || !hasAllowed(g, "10.0.0.8/32") {
		t.Errorf("relayed /32 must merge into lowest-UUID gateway entry: %+v", out)
	}
	if findPeer(out, "p8") != nil {
		t.Error("relayed NAT'd peer must not get a direct entry")
	}
}

func TestRouting_EmptyIntersectionOmits(t *testing.T) {
	self := RoutingNode{NodeID: id(9), OverlayIP: ip("10.0.0.9")}
	peer := RoutingNode{NodeID: id(8), PublicKey: "p8", OverlayIP: ip("10.0.0.8")}
	if out := ComputeWireGuardRouting(self, []RoutingNode{peer}); len(out) != 0 {
		t.Errorf("no common gateway => omit, got %+v", out)
	}
}
