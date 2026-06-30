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

// mkKindNode creates a ready node of the given kind with a pool on the shared
// name, returning the node id. Both the node-kind and gateway-kind rows are made
// ready and given a pool so the placement query would surface either - the only
// thing that may drop the gateway is the kind discriminator, not a missing pool
// or a non-ready status.
func mkKindNode(t *testing.T, s *etcdstore.Store, prefix, kind, poolName string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	np := nodeParams(uniqueNodeName(prefix))
	np.Kind = kind
	if _, err := s.CreateNode(ctx, np); err != nil {
		t.Fatalf("CreateNode(%s): %v", kind, err)
	}
	if _, err := s.UncordonNode(ctx, np.ID); err != nil {
		t.Fatalf("UncordonNode(%s): %v", kind, err)
	}
	if _, err := s.CreateStoragePool(ctx, poolParams(np.ID, poolName)); err != nil {
		t.Fatalf("CreateStoragePool(%s): %v", kind, err)
	}
	return np.ID
}

// TestPlacementExcludesGatewayKind drives the real placement querier: a node-kind
// node and a gateway-kind node both hold a ready pool on the same name, yet the
// eligible-pool enumeration must surface only the node-kind node's pair.
func TestPlacementExcludesGatewayKind(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	poolName := uniquePoolName("gw-excl")
	nodeID := mkKindNode(t, s, "node", store.NodeKindNode, poolName)
	gwID := mkKindNode(t, s, "gw", store.NodeKindGateway, poolName)

	rows, err := s.PlacementQuerier().ListEligiblePoolsByName(ctx, poolName)
	if err != nil {
		t.Fatalf("ListEligiblePoolsByName: %v", err)
	}
	var got []uuid.UUID
	for _, r := range rows {
		got = append(got, r.NodeEffectiveAvailability.ID)
	}
	want := []uuid.UUID{nodeID}
	if len(got) != 1 || got[0] != nodeID {
		t.Errorf("eligible pool nodes = %v, want %v (gateway %v must be excluded)", got, want, gwID)
	}
}

// TestNodeByNameRoundTripsKind confirms Kind persists and reads back, and that an
// unset kind defaults to the node kind.
func TestNodeByNameRoundTripsKind(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	gwName := uniqueNodeName("gw")
	gwp := nodeParams(gwName)
	gwp.Kind = store.NodeKindGateway
	if _, err := s.CreateNode(ctx, gwp); err != nil {
		t.Fatalf("CreateNode(gateway): %v", err)
	}
	gw, err := s.NodeByName(ctx, gwName)
	if err != nil {
		t.Fatalf("NodeByName(gateway): %v", err)
	}
	if gw.Kind != store.NodeKindGateway {
		t.Errorf("gateway Kind = %q, want %q", gw.Kind, store.NodeKindGateway)
	}

	defName := uniqueNodeName("def")
	if _, err := s.CreateNode(ctx, nodeParams(defName)); err != nil {
		t.Fatalf("CreateNode(default): %v", err)
	}
	def, err := s.NodeByName(ctx, defName)
	if err != nil {
		t.Fatalf("NodeByName(default): %v", err)
	}
	if def.Kind != store.NodeKindNode {
		t.Errorf("default Kind = %q, want %q (empty kind defaults to node)", def.Kind, store.NodeKindNode)
	}
}
