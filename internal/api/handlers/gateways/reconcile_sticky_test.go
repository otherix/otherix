// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package gateways

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// goneGatewayNode is a gateway node the liveness check treats as dead: a node in
// the terminal-or-stale 'gone' status can no longer carry traffic, so it cannot
// hold a live session.
func goneGatewayNode(id uuid.UUID) store.Node {
	return store.Node{ID: id, Kind: store.NodeKindGateway, Status: store.NodeStatusGone}
}

// sessionStatus builds a per-(node, network) status row reporting count live
// ingress sessions, the gateway-self-reported signal the sticky guard reads.
func sessionStatus(networkID, nodeID uuid.UUID, count int) store.NetworkNodeStatus {
	return store.NetworkNodeStatus{
		NetworkID:            networkID,
		NodeID:               nodeID,
		ReconciliationStatus: "ready",
		ActiveSessions:       count,
	}
}

// TestReconcileKeepsMembershipWithLiveSessions covers the sticky guard: a live
// gateway whose membership network has gone ingress-inactive (its last VM NIC
// removed) is NOT reaped while the gateway still reports a live session draining
// on it. Reaping it would yank the session's coverage out from under it.
//
// Revert-to-confirm: drop the active_sessions>0 branch from reapNetwork and this
// test fails (the membership is reaped) - proof the guard carries its weight.
func TestReconcileKeepsMembershipWithLiveSessions(t *testing.T) {
	netID := uuid.New()
	g1 := uuid.New()
	f := &reconcileStoreFake{
		networks:      []store.Network{{ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		nicsByNetwork: map[uuid.UUID][]store.VMNic{}, // ingress-inactive: 0 VM NICs
		nodes:         []store.Node{gatewayNode(g1)},
		memberships: map[uuid.UUID][]store.GatewayMembership{
			netID: {{GatewayID: g1, NetworkID: netID}},
		},
		statusByNetwork: map[uuid.UUID][]store.NetworkNodeStatus{
			netID: {sessionStatus(netID, g1, 1)},
		},
	}

	if err := ReconcileFunc(f, ReconcileConfig{}, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("reaped %d memberships while a session was live, want 0", len(f.deleted))
	}
	if got := len(f.memberships[netID]); got != 1 {
		t.Fatalf("membership count = %d, want 1 (kept sticky)", got)
	}
}

// TestReconcileReapsIdleMembershipOnInactiveNetwork covers the reap path: a live
// gateway reporting zero sessions on an ingress-inactive network is reaped, so a
// membership does not leak forever once its network goes idle.
func TestReconcileReapsIdleMembershipOnInactiveNetwork(t *testing.T) {
	netID := uuid.New()
	g1 := uuid.New()
	f := &reconcileStoreFake{
		networks:      []store.Network{{ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		nicsByNetwork: map[uuid.UUID][]store.VMNic{}, // ingress-inactive
		nodes:         []store.Node{gatewayNode(g1)},
		memberships: map[uuid.UUID][]store.GatewayMembership{
			netID: {{GatewayID: g1, NetworkID: netID}},
		},
		statusByNetwork: map[uuid.UUID][]store.NetworkNodeStatus{
			netID: {sessionStatus(netID, g1, 0)},
		},
	}

	if err := ReconcileFunc(f, ReconcileConfig{}, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.deleted) != 1 || f.deleted[0].gateway != g1 || f.deleted[0].network != netID {
		t.Fatalf("deleted = %+v, want exactly the (g1, netID) membership", f.deleted)
	}
	if got := len(f.memberships[netID]); got != 0 {
		t.Fatalf("membership count = %d, want 0 (reaped)", got)
	}
}

// TestReconcileReapsDeadGatewayRegardlessOfStaleCount covers the dead-gateway
// path: a gateway in 'gone'/'unreachable' is reaped REGARDLESS of its
// last-reported session count. A dead gateway cannot hold a live session, and a
// stale count must never wedge the reaper forever.
func TestReconcileReapsDeadGatewayRegardlessOfStaleCount(t *testing.T) {
	netID := uuid.New()
	dead := uuid.New()
	f := &reconcileStoreFake{
		networks: []store.Network{{ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		// Network still ingress-active, yet the gateway is dead: the dead-gateway
		// path reaps regardless of network activity AND regardless of the count.
		nicsByNetwork: map[uuid.UUID][]store.VMNic{
			netID: {{ID: uuid.New(), NetworkID: netID}},
		},
		nodes: []store.Node{goneGatewayNode(dead)},
		memberships: map[uuid.UUID][]store.GatewayMembership{
			netID: {{GatewayID: dead, NetworkID: netID}},
		},
		statusByNetwork: map[uuid.UUID][]store.NetworkNodeStatus{
			netID: {sessionStatus(netID, dead, 5)}, // stale, must not wedge the reaper
		},
	}

	if err := ReconcileFunc(f, ReconcileConfig{}, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.deleted) != 1 || f.deleted[0].gateway != dead {
		t.Fatalf("deleted = %+v, want the dead gateway's membership reaped regardless of stale count", f.deleted)
	}
}

// TestReconcileNeverReapsActiveNetworkCoverage is the no-blackhole guard: an
// ingress-active overlay covered by live gateways keeps every membership. The
// reaping pass touches only inactive-network and dead-gateway memberships, never
// the live coverage the additive pass maintains.
func TestReconcileNeverReapsActiveNetworkCoverage(t *testing.T) {
	netID := uuid.New()
	g1, g2 := uuid.New(), uuid.New()
	f := &reconcileStoreFake{
		networks: []store.Network{{ID: netID, Type: store.NetworkTypeOverlay, VNI: vni(100)}},
		nicsByNetwork: map[uuid.UUID][]store.VMNic{
			netID: {{ID: uuid.New(), NetworkID: netID}},
		},
		nodes: []store.Node{gatewayNode(g1), gatewayNode(g2)},
		memberships: map[uuid.UUID][]store.GatewayMembership{
			netID: {{GatewayID: g1, NetworkID: netID}, {GatewayID: g2, NetworkID: netID}},
		},
		statusByNetwork: map[uuid.UUID][]store.NetworkNodeStatus{
			netID: {sessionStatus(netID, g1, 0), sessionStatus(netID, g2, 0)},
		},
	}

	if err := ReconcileFunc(f, ReconcileConfig{}, discardLog())(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("reaped %d memberships on an active network, want 0", len(f.deleted))
	}
	if got := len(f.memberships[netID]); got != 2 {
		t.Fatalf("membership count = %d, want 2 (coverage untouched)", got)
	}
}
