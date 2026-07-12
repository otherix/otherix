// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// seedWG records an agent WireGuard identity for a node. An empty endpoint marks
// the node NAT'd (no directly-dialable WG endpoint), matching the agent-reported
// signal the routing producer and agentclient resolver key on.
func seedWG(t *testing.T, s *etcdstore.Store, nodeID uuid.UUID, endpoint string, peers []string) {
	t.Helper()
	err := s.UpsertAgentWireguard(context.Background(), store.UpsertAgentWireguardParams{
		NodeID:           nodeID,
		PublicKey:        "pk-" + nodeID.String(),
		Endpoint:         endpoint,
		EstablishedPeers: peers,
	})
	if err != nil {
		t.Fatalf("UpsertAgentWireguard(%v): %v", nodeID, err)
	}
}

// eligibleIDs runs the real placement querier for a pool name and returns the
// set of node ids that surfaced as candidates.
func eligibleIDs(t *testing.T, s *etcdstore.Store, poolName string) map[uuid.UUID]bool {
	t.Helper()
	rows, err := s.PlacementQuerier().ListEligiblePoolsByName(context.Background(), poolName)
	if err != nil {
		t.Fatalf("ListEligiblePoolsByName: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, r := range rows {
		got[r.NodeEffectiveAvailability.ID] = true
	}
	return got
}

// TestDownPathGate_TeethNATdNodeWithNoLiveGateway is the load-bearing teeth: a
// NAT'd mesh node that no live gateway lists in its established peers is NOT a
// placement candidate, even though it is ready and owns a pool.
// Revert-to-confirm: delete the downPathReachable AND in poolNodePairs and the
// NAT'd node IS a candidate (it passes every other predicate).
func TestDownPathGate_TeethNATdNodeWithNoLiveGateway(t *testing.T) {
	s, _ := startStore(t)
	poolName := uniquePoolName("downpath-teeth")
	natd := mkKindNode(t, s, "natd", store.NodeKindNode, poolName)
	seedWG(t, s, natd, "", nil) // NAT'd: empty endpoint
	// A live gateway exists but lists no peers, so it relays no one.
	gw := mkGatewayNodeNoPool(t, s, "gw")
	seedWG(t, s, gw, "gw.example:51820", nil)

	if eligibleIDs(t, s, poolName)[natd] {
		t.Errorf("NAT'd node %v with no live gateway relaying it must not be a placement candidate", natd)
	}
}

// TestDownPathGate_ReachableNATdNode is the same NAT'd node, now relayed: a
// gateway reports it in EstablishedPeers with a fresh UpdatedAt, so it IS a
// candidate.
func TestDownPathGate_ReachableNATdNode(t *testing.T) {
	s, _ := startStore(t)
	poolName := uniquePoolName("downpath-reach")
	natd := mkKindNode(t, s, "natd", store.NodeKindNode, poolName)
	seedWG(t, s, natd, "", nil)
	gw := mkGatewayNodeNoPool(t, s, "gw")
	seedWG(t, s, gw, "gw.example:51820", []string{natd.String()}) // fresh handshake

	if !eligibleIDs(t, s, poolName)[natd] {
		t.Errorf("NAT'd node %v with a fresh gateway handshake must be a placement candidate", natd)
	}
}

// TestDownPathGate_DoesNotFlipNodeHealth guards the orthogonality contract: a
// down-path-dead NAT'd node stays NodeStatus=ready and uncordoned. The gate is
// placement-only and never writes node health.
func TestDownPathGate_DoesNotFlipNodeHealth(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	poolName := uniquePoolName("downpath-orth")
	natd := mkKindNode(t, s, "natd", store.NodeKindNode, poolName)
	seedWG(t, s, natd, "", nil) // NAT'd, no gateway relays it -> down-path dead

	_ = eligibleIDs(t, s, poolName) // exercise the gate

	n, err := s.NodeByID(ctx, natd)
	if err != nil {
		t.Fatalf("NodeByID(%v): %v", natd, err)
	}
	if n.Status != store.NodeStatusReady {
		t.Errorf("down-path-dead node status = %q, want %q (gate must not touch health)", n.Status, store.NodeStatusReady)
	}
	if n.CordonedAt != nil {
		t.Errorf("down-path-dead node CordonedAt = %v, want nil (gate must not cordon)", n.CordonedAt)
	}
}

// TestDownPathGate_FailOpenNoRecord is the fail-open contract: a node with NO
// AgentWireguard record at all (non-mesh cluster, or a node that has not sent its
// first WG heartbeat) stays schedulable.
// Revert-to-confirm: a buggy "no record => NAT'd" classification would drop it.
func TestDownPathGate_FailOpenNoRecord(t *testing.T) {
	s, _ := startStore(t)
	poolName := uniquePoolName("downpath-norecord")
	plain := mkKindNode(t, s, "plain", store.NodeKindNode, poolName) // no seedWG

	if !eligibleIDs(t, s, poolName)[plain] {
		t.Errorf("node %v with no WireGuard record must stay schedulable (fail open)", plain)
	}
}

// TestDownPathGate_PublicNodeReachable confirms a node the CP dials directly (WG
// record with a non-empty endpoint) is a candidate regardless of any gateway.
func TestDownPathGate_PublicNodeReachable(t *testing.T) {
	s, _ := startStore(t)
	poolName := uniquePoolName("downpath-public")
	pub := mkKindNode(t, s, "pub", store.NodeKindNode, poolName)
	seedWG(t, s, pub, "pub.example:51820", nil) // public: non-empty endpoint

	if !eligibleIDs(t, s, poolName)[pub] {
		t.Errorf("public node %v must be a placement candidate", pub)
	}
}
