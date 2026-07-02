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

// TestPlacementExcludesGatewayRole drives the real placement querier through the
// role predicate: a hypervisor node and a gateway node both hold a ready pool on
// the same name, yet the eligible-pool enumeration must surface only the
// hypervisor node's pair because the querier now excludes any node that does not
// hold the hypervisor role.
func TestPlacementExcludesGatewayRole(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	poolName := uniquePoolName("gw-role-excl")
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
	if len(got) != 1 || got[0] != nodeID {
		t.Errorf("eligible pool nodes = %v, want [%v] (gateway %v excluded)", got, nodeID, gwID)
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
