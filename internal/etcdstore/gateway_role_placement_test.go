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
