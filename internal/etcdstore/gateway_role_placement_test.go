// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// TestPlacementExcludesGatewayRole drives the real placement querier through
// pool ownership: a hypervisor node and a co-located node (gateway bit + a pool
// on the same name) both surface as eligible pairs, while a gateway-only node
// with no pool never produces a pair. Pool ownership is the schedulability gate;
// the gateway bit alone no longer excludes.
func TestPlacementExcludesGatewayRole(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	poolName := uniquePoolName("gw-role-excl")
	hvID := mkKindNode(t, s, "hv", store.NodeKindNode, poolName)    // pool, no gateway bit -> [hypervisor]
	coID := mkKindNode(t, s, "co", store.NodeKindGateway, poolName) // pool + gateway bit -> [hypervisor,gateway]
	gwOnly := mkGatewayNodeNoPool(t, s, "gw")                       // gateway bit, no pool -> [gateway]

	rows, err := s.PlacementQuerier().ListEligiblePoolsByName(ctx, poolName)
	if err != nil {
		t.Fatalf("ListEligiblePoolsByName: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, r := range rows {
		got[r.NodeEffectiveAvailability.ID] = true
	}
	if !got[hvID] {
		t.Errorf("hypervisor node %v missing from eligible pools %v", hvID, got)
	}
	if !got[coID] {
		t.Errorf("co-located node %v missing from eligible pools %v (must stay schedulable)", coID, got)
	}
	if got[gwOnly] {
		t.Errorf("gateway-only node %v (no pool) must not be eligible, got %v", gwOnly, got)
	}
}

// TestListNodesEffectiveRoleFilter drives the server-side role filter on the
// list querier: with Role=gateway only the gateway node surfaces, the
// hypervisor node is excluded. This keeps cursor pagination honest (the filter
// runs in the store, not client-side).
func TestListNodesEffectiveRoleFilter(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	poolName := uniquePoolName("role-filter")
	nodeID := mkKindNode(t, s, "node", store.NodeKindNode, poolName)
	gwID := mkKindNode(t, s, "gw", store.NodeKindGateway, poolName)

	gw := store.NodeRoleGateway
	rows, err := s.ListNodesEffective(ctx, store.ListNodesEffectiveParams{Role: &gw, LimitCount: 100})
	if err != nil {
		t.Fatalf("ListNodesEffective(role=gateway): %v", err)
	}
	if len(rows) != 1 || rows[0].ID != gwID {
		t.Errorf("role=gateway -> %d rows, want only gateway %v (hypervisor %v excluded)", len(rows), gwID, nodeID)
	}
}
