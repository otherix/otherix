// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// seedWG records an agent WireGuard identity for a node. An empty endpoint marks
// the node NAT'd (no directly-dialable WG endpoint), matching the agent-reported
// signal the routing producer and agentclient resolver key on. peers is the
// node's own reported established-handshake set - the same set agentroute's
// selectGateway reads to pick a route.
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

// seedWGStaleGateway seeds a NAT'd node whose own established peers list gateway
// but whose report is backdated by age, so the down-path freshness bound can be
// exercised. It writes through the normal upsert (to allocate the overlay index),
// then rewrites the primary record with the backdated UpdatedAt via the raw
// client, since UpsertAgentWireguard always stamps the report at now.
func seedWGStaleGateway(t *testing.T, s *etcdstore.Store, c *etcd.Client, nodeID, gateway uuid.UUID, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	seedWG(t, s, nodeID, "", []string{gateway.String()})
	rec, err := s.AgentWireguardByNodeID(ctx, nodeID)
	if err != nil {
		t.Fatalf("AgentWireguardByNodeID(%v): %v", nodeID, err)
	}
	rec.UpdatedAt = time.Now().UTC().Add(-age)
	if err := c.PutJSON(ctx, etcd.Key("agent_wireguard", nodeID.String()), rec); err != nil {
		t.Fatalf("backdate agent_wireguard(%v): %v", nodeID, err)
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

// TestDownPathGate_TeethNATdNodeListsNoGateway is the load-bearing teeth: a NAT'd
// mesh node whose own reported handshake set lists no public gateway is NOT a
// placement candidate, even though it is ready and owns a pool - the CP has no
// wired route to drive it. This mirrors agentroute selectGateway, which routes
// off the NAT'd node's OWN established peers.
// Revert-to-confirm: forcing natd=false in poolNodePairs makes it a candidate (it
// passes every other predicate).
func TestDownPathGate_TeethNATdNodeListsNoGateway(t *testing.T) {
	s, _ := startStore(t)
	poolName := uniquePoolName("downpath-teeth")
	natd := mkKindNode(t, s, "natd", store.NodeKindNode, poolName)
	seedWG(t, s, natd, "", nil) // NAT'd, own peers list no gateway
	// A public gateway exists, but the NAT'd node does not list it in its own
	// established peers, so the CP resolver would find no route.
	_ = mkGatewayNodeNoPool(t, s, "gw")

	if eligibleIDs(t, s, poolName)[natd] {
		t.Errorf("NAT'd node %v that lists no public gateway must not be a placement candidate", natd)
	}
}

// TestDownPathGate_ReachableNATdNode is the same NAT'd node, now routable: its own
// established peers list a public gateway (gateway role + advertised endpoint), so
// the CP can drive it and it IS a candidate.
func TestDownPathGate_ReachableNATdNode(t *testing.T) {
	s, _ := startStore(t)
	poolName := uniquePoolName("downpath-reach")
	natd := mkKindNode(t, s, "natd", store.NodeKindNode, poolName)
	gw := mkGatewayNodeNoPool(t, s, "gw")
	seedWG(t, s, natd, "", []string{gw.String()}) // own peers list the public gateway

	if !eligibleIDs(t, s, poolName)[natd] {
		t.Errorf("NAT'd node %v that lists a public gateway must be a placement candidate", natd)
	}
}

// TestDownPathGate_StaleOwnReport confirms the freshness bound has teeth: a NAT'd
// node that DOES list a public gateway but whose own report is older than
// DownPathStaleness is NOT a candidate.
func TestDownPathGate_StaleOwnReport(t *testing.T) {
	s, c := startStore(t)
	poolName := uniquePoolName("downpath-stale")
	natd := mkKindNode(t, s, "natd", store.NodeKindNode, poolName)
	gw := mkGatewayNodeNoPool(t, s, "gw")
	// Default DownPathStaleness is 90s; backdate the node's own report well past it.
	seedWGStaleGateway(t, s, c, natd, gw, 2*time.Minute)

	if eligibleIDs(t, s, poolName)[natd] {
		t.Errorf("NAT'd node %v with a stale own report must not be a placement candidate", natd)
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
	seedWG(t, s, natd, "", nil) // NAT'd, lists no gateway -> down-path dead

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
// record with a non-empty endpoint) is a candidate regardless of its peer set.
func TestDownPathGate_PublicNodeReachable(t *testing.T) {
	s, _ := startStore(t)
	poolName := uniquePoolName("downpath-public")
	pub := mkKindNode(t, s, "pub", store.NodeKindNode, poolName)
	seedWG(t, s, pub, "pub.example:51820", nil) // public: non-empty endpoint

	if !eligibleIDs(t, s, poolName)[pub] {
		t.Errorf("public node %v must be a placement candidate", pub)
	}
}
