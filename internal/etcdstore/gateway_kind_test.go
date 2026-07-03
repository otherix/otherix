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
// name, returning the node id. The kind argument maps to the create-param
// gateway bit (NodeKindGateway -> Gateway=true). Both the hypervisor and gateway
// rows are made ready and given a pool so the placement query would surface
// either - the only thing that may drop the gateway is its role, not a missing
// pool or a non-ready status.
func mkKindNode(t *testing.T, s *etcdstore.Store, prefix, kind, poolName string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	np := nodeParams(uniqueNodeName(prefix))
	np.Gateway = kind == store.NodeKindGateway
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

// mkGatewayNodeNoPool creates a ready node with the gateway bit and NO storage
// pool, returning the node id. This is a standalone gateway: it owns no pool, so
// it derives no hypervisor role and is never VM-schedulable.
func mkGatewayNodeNoPool(t *testing.T, s *etcdstore.Store, prefix string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	np := nodeParams(uniqueNodeName(prefix))
	np.Gateway = true
	if _, err := s.CreateNode(ctx, np); err != nil {
		t.Fatalf("CreateNode(gateway-no-pool): %v", err)
	}
	if _, err := s.UncordonNode(ctx, np.ID); err != nil {
		t.Fatalf("UncordonNode(gateway-no-pool): %v", err)
	}
	return np.ID
}

// TestNodeByNameRoundTripsGatewayRole confirms the gateway role persists and
// reads back, and that a node created without the gateway bit reads back as a
// hypervisor.
func TestNodeByNameRoundTripsGatewayRole(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	gwName := uniqueNodeName("gw")
	gwp := nodeParams(gwName)
	gwp.Gateway = true
	if _, err := s.CreateNode(ctx, gwp); err != nil {
		t.Fatalf("CreateNode(gateway): %v", err)
	}
	gw, err := s.NodeByName(ctx, gwName)
	if err != nil {
		t.Fatalf("NodeByName(gateway): %v", err)
	}
	if !gw.HasRole(store.NodeRoleGateway) {
		t.Errorf("gateway node roles = %v, want [gateway]", gw.Roles())
	}

	defName := uniqueNodeName("def")
	if _, err := s.CreateNode(ctx, nodeParams(defName)); err != nil {
		t.Fatalf("CreateNode(default): %v", err)
	}
	def, err := s.NodeByName(ctx, defName)
	if err != nil {
		t.Fatalf("NodeByName(default): %v", err)
	}
	if !def.HasRole(store.NodeRoleHypervisor) {
		t.Errorf("default node roles = %v, want [hypervisor] (no gateway bit)", def.Roles())
	}
}
