// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"net/netip"
	"sort"

	"github.com/google/uuid"
)

// routingNode is one node's WireGuard-fabric identity as computeWireGuardRouting
// sees it: the CP-assigned overlay address and public key, the node's advertised
// WireGuard endpoint ("" when the node is behind NAT), whether it holds the
// ingress-gateway role, and the set of node-ids it currently has a live
// handshake with (the reachability source, derived from the persisted
// EstablishedPeers).
type routingNode struct {
	NodeID           uuid.UUID
	PublicKey        string
	OverlayIP        netip.Addr
	Endpoint         string // advertised WireGuard host:port; "" if NAT'd
	IsGateway        bool
	EstablishedPeers map[uuid.UUID]bool
}

// computeWireGuardRouting returns self's declared WireGuard peer set and the
// node-ids it could not route. It is pure and deterministic - no clock, no store,
// no randomness - so it is unit-testable in isolation and yields a stable result
// across heartbeats. For each peer it emits one of:
//
//   - a direct entry when the peer advertises a WireGuard endpoint (a public node
//     or a gateway): AllowedIPs is the peer's overlay /32;
//   - when self is a gateway and the peer is NAT'd: an endpoint-less direct entry,
//     declared unconditionally (not gated on an existing handshake) so the gateway
//     has the peer configured before the peer dials in - WireGuard silently drops a
//     handshake from an unconfigured peer, so gating this on self.EstablishedPeers
//     would deadlock a cold start (the gateway would never accept the very handshake
//     that would add the peer to EstablishedPeers). The endpoint is learned by
//     roaming from the peer's inbound handshake;
//   - for any other NAT'd peer: the peer's overlay /32 merged into the relaying
//     gateway's AllowedIPs (the lowest-UUID gateway both self and the peer
//     currently reach). When no such gateway exists the peer is omitted - a route
//     is never half-wired - and its node-id is returned in omitted so the caller
//     can surface the connectivity black hole.
func computeWireGuardRouting(self routingNode, peers []routingNode) ([]declaredWireGuardPeer, []uuid.UUID) {
	// entries is keyed by peer node-id so a relayed /32 can be merged into the
	// relaying gateway's already-emitted direct entry regardless of peer order.
	entries := make(map[uuid.UUID]*declaredWireGuardPeer, len(peers))
	var gateways []routingNode
	var relayed []routingNode
	var omitted []uuid.UUID

	for _, p := range peers {
		if p.IsGateway && p.Endpoint != "" {
			// Only an endpoint-bearing gateway can be a relay hub: it has a direct
			// entry a relayed /32 can merge into.
			gateways = append(gateways, p)
		}
		switch {
		case p.Endpoint != "": // reachable peer -> direct entry
			entries[p.NodeID] = directEntry(p, p.Endpoint)
		case self.IsGateway: // self is a gateway: pre-declare every NAT'd peer endpoint-less
			// so the gateway can accept the peer's inbound handshake (WireGuard drops a
			// handshake from an unconfigured peer). Unconditional - gating on an existing
			// handshake would deadlock a cold start. The endpoint is learned by roaming.
			entries[p.NodeID] = directEntry(p, "")
		default: // NAT'd peer relayed through a gateway
			relayed = append(relayed, p)
		}
	}

	for _, p := range relayed {
		g, ok := selectRelayGateway(self, p, gateways)
		if !ok {
			omitted = append(omitted, p.NodeID) // no common gateway -> omit (never half-wire)
			continue
		}
		// g came from gateways (endpoint-bearing) so it always has a rule-1 direct
		// entry to merge into.
		e := entries[g.NodeID]
		e.AllowedIPs = append(e.AllowedIPs, netip.PrefixFrom(p.OverlayIP, 32).String())
	}

	out := make([]declaredWireGuardPeer, 0, len(entries))
	for _, e := range entries {
		e.AllowedIPs = sortDedup(e.AllowedIPs)
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, omitted
}

// selectRelayGateway returns the lowest-UUID gateway both a and b currently reach
// (present in both EstablishedPeers sets), or ok=false when the intersection is
// empty. The deterministic node-id tie-break keeps the relay choice stable across
// heartbeats so both ends of a pair pick the same hub.
func selectRelayGateway(a, b routingNode, gateways []routingNode) (routingNode, bool) {
	var best routingNode
	found := false
	for _, g := range gateways {
		if !a.EstablishedPeers[g.NodeID] || !b.EstablishedPeers[g.NodeID] {
			continue
		}
		if !found || g.NodeID.String() < best.NodeID.String() {
			best, found = g, true
		}
	}
	return best, found
}

// directEntry builds a declared peer with the peer's overlay /32 as its sole
// AllowedIPs and the given endpoint (empty when WireGuard must learn it by
// roaming from the peer's inbound handshake).
func directEntry(p routingNode, endpoint string) *declaredWireGuardPeer {
	return &declaredWireGuardPeer{
		NodeID:     p.NodeID.String(),
		PublicKey:  p.PublicKey,
		Endpoint:   endpoint,
		OverlayIP:  p.OverlayIP.String(),
		AllowedIPs: []string{netip.PrefixFrom(p.OverlayIP, 32).String()},
	}
}

// sortDedup returns the CIDRs sorted and de-duplicated in place.
func sortDedup(cidrs []string) []string {
	if len(cidrs) < 2 {
		return cidrs
	}
	sort.Strings(cidrs)
	out := cidrs[:1]
	for _, c := range cidrs[1:] {
		if c != out[len(out)-1] {
			out = append(out, c)
		}
	}
	return out
}
