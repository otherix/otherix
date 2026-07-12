// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"fmt"
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
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
	self := routingNode{NodeID: id(1), OverlayIP: ip("10.0.0.1")} // NAT'd
	pub := routingNode{NodeID: id(2), PublicKey: "p2", OverlayIP: ip("10.0.0.2"), Endpoint: "5.5.5.5:51820"}
	out, _ := computeWireGuardRouting(self, []routingNode{pub})
	if len(out) != 1 || out[0].Endpoint != "5.5.5.5:51820" || out[0].AllowedIPs[0] != "10.0.0.2/32" {
		t.Fatalf("direct wrong: %+v", out)
	}
}

func TestRouting_GatewayForwardsToDialer(t *testing.T) {
	// self is a gateway; each NAT'd dialer in EstablishedPeers gets an
	// endpoint-less direct entry (WireGuard learns the endpoint by roaming), and
	// a second dialer must not overwrite the first.
	tests := []struct {
		name    string
		dialers []routingNode
		wantIPs map[string]string // pubkey -> expected endpoint-less /32
	}{
		{
			name:    "single dialer",
			dialers: []routingNode{{NodeID: id(9), PublicKey: "p9", OverlayIP: ip("10.0.0.9")}},
			wantIPs: map[string]string{"p9": "10.0.0.9/32"},
		},
		{
			name: "two dialers, neither overwrites the other",
			dialers: []routingNode{
				{NodeID: id(8), PublicKey: "p8", OverlayIP: ip("10.0.0.8")},
				{NodeID: id(9), PublicKey: "p9", OverlayIP: ip("10.0.0.9")},
			},
			wantIPs: map[string]string{"p8": "10.0.0.8/32", "p9": "10.0.0.9/32"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			established := map[uuid.UUID]bool{}
			for _, d := range tt.dialers {
				established[d.NodeID] = true
			}
			self := routingNode{
				NodeID: id(2), OverlayIP: ip("10.0.0.2"), Endpoint: "2.2.2.2:51820",
				IsGateway: true, EstablishedPeers: established,
			}
			out, _ := computeWireGuardRouting(self, tt.dialers)
			if len(out) != len(tt.wantIPs) {
				t.Fatalf("want %d forward entries, got %d: %+v", len(tt.wantIPs), len(out), out)
			}
			for pk, cidr := range tt.wantIPs {
				e := findPeer(out, pk)
				if e == nil || e.Endpoint != "" || !hasAllowed(e, cidr) {
					t.Errorf("dialer %s must get an endpoint-less %s entry: %+v", pk, cidr, out)
				}
			}
		})
	}
}

// TestRouting_GatewayDeclaresNATdPeerBeforeHandshake pins the cold-start fix: a
// gateway must declare a NAT'd peer endpoint-less even when it has NO live
// handshake with it yet (the peer is absent from EstablishedPeers). WireGuard
// drops a handshake from an unconfigured peer, so gating this on an existing
// handshake deadlocks a cold start - the gateway would never accept the very
// handshake that would add the peer to EstablishedPeers.
func TestRouting_GatewayDeclaresNATdPeerBeforeHandshake(t *testing.T) {
	// self is a gateway that has established with nobody yet.
	self := routingNode{
		NodeID: id(2), OverlayIP: ip("10.0.0.2"), Endpoint: "2.2.2.2:51820",
		IsGateway: true, EstablishedPeers: map[uuid.UUID]bool{},
	}
	natd := routingNode{NodeID: id(9), PublicKey: "p9", OverlayIP: ip("10.0.0.9")} // NAT'd: no endpoint
	out, omitted := computeWireGuardRouting(self, []routingNode{natd})
	if len(omitted) != 0 {
		t.Errorf("gateway must not omit a NAT'd peer it has to accept a handshake from; omitted=%v", omitted)
	}
	e := findPeer(out, "p9")
	if e == nil || e.Endpoint != "" || !hasAllowed(e, "10.0.0.9/32") {
		t.Errorf("gateway must pre-declare the NAT'd peer as an endpoint-less 10.0.0.9/32 entry before any handshake: %+v", out)
	}
}

func TestRouting_RelayedMergeLowestUUID(t *testing.T) {
	self := routingNode{NodeID: id(9), OverlayIP: ip("10.0.0.9"), EstablishedPeers: map[uuid.UUID]bool{id(2): true, id(3): true}}
	peer := routingNode{NodeID: id(8), PublicKey: "p8", OverlayIP: ip("10.0.0.8"), EstablishedPeers: map[uuid.UUID]bool{id(2): true, id(3): true}}
	g1 := routingNode{NodeID: id(2), PublicKey: "g2", OverlayIP: ip("10.0.0.2"), Endpoint: "2.2.2.2:51820", IsGateway: true}
	g2 := routingNode{NodeID: id(3), PublicKey: "g3", OverlayIP: ip("10.0.0.3"), Endpoint: "3.3.3.3:51820", IsGateway: true}
	out, _ := computeWireGuardRouting(self, []routingNode{peer, g1, g2})
	g := findPeer(out, "g2") // lowest UUID id(2)
	if g == nil || !hasAllowed(g, "10.0.0.2/32") || !hasAllowed(g, "10.0.0.8/32") {
		t.Errorf("relayed /32 must merge into lowest-UUID gateway entry: %+v", out)
	}
	if findPeer(out, "p8") != nil {
		t.Error("relayed NAT'd peer must not get a direct entry")
	}
}

func TestRouting_EmptyIntersectionOmits(t *testing.T) {
	self := routingNode{NodeID: id(9), OverlayIP: ip("10.0.0.9")}
	peer := routingNode{NodeID: id(8), PublicKey: "p8", OverlayIP: ip("10.0.0.8")}
	out, omitted := computeWireGuardRouting(self, []routingNode{peer})
	if len(out) != 0 {
		t.Errorf("no common gateway => omit, got %+v", out)
	}
	if diff := cmp.Diff([]uuid.UUID{id(8)}, omitted); diff != "" {
		t.Errorf("omitted node-ids mismatch (-want +got):\n%s", diff)
	}
}
