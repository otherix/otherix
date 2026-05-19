// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"testing"

	"github.com/otherix/otherix/internal/store"
)

// TestE2E_PoolReconciliation_DeletionPropagates confirms что soft-
// deleting а storage_pools row drops it от the heartbeat's
// declared_pools list on the next cycle. The agent
// reconciler observes the absence и unregisters from its local
// map; filesystem state is left intact. This test exercises
// only the CP-side surface — the absence-from-declared_pools is the
// signal the agent acts on.
func TestE2E_PoolReconciliation_DeletionPropagates(t *testing.T) {
	h := newCPAgentHarness(t)
	ctx := context.Background()

	node := seedNodeWithCert(t, h, store.CpuArchAmd64, store.NodeStatusReady, false)
	pool := seedPoolDirect(t, h, node, "ephemeral", "/opt/otherix/pools/ephemeral")

	// --- Cycle 1: row alive, declared_pools includes it.
	resp1 := postHeartbeatExpect200(t, ctx, h, node.Name, heartbeatBodyWithPools("amd64", nil))
	if len(resp1.DeclaredPools) != 1 || resp1.DeclaredPools[0].Name != "ephemeral" {
		t.Fatalf("cycle 1 declared_pools = %+v, want one entry named ephemeral", resp1.DeclaredPools)
	}

	// --- Soft-delete the row directly through the sqlc query.
	if err := h.store.Queries().SoftDeleteStoragePool(ctx, pool.ID); err != nil {
		t.Fatalf("SoftDeleteStoragePool: %v", err)
	}

	// --- Cycle 2: row soft-deleted, declared_pools is empty.
	resp2 := postHeartbeatExpect200(t, ctx, h, node.Name, heartbeatBodyWithPools("amd64", nil))
	if got := len(resp2.DeclaredPools); got != 0 {
		t.Errorf("cycle 2 declared_pools count = %d, want 0 (got %+v)", got, resp2.DeclaredPools)
	}
}

// TestE2E_PoolReconciliation_MultiNodeFilter confirms что the CP
// returns only the per-node pool inventory: pools owned by а
// different node MUST NOT appear in this node's declared_pools.
// SL15 invariant — multi-instance pool names disambiguate per-node
// on the CP side, agents see only their own row.
func TestE2E_PoolReconciliation_MultiNodeFilter(t *testing.T) {
	h := newCPAgentHarness(t)
	ctx := context.Background()

	nodeA := seedNodeWithCert(t, h, store.CpuArchAmd64, store.NodeStatusReady, false)
	nodeB := seedNodeOnly(t, h, store.CpuArchAmd64, store.NodeStatusReady)

	_ = seedPoolDirect(t, h, nodeA, "shared-name", "/opt/otherix/pools/A")
	_ = seedPoolDirect(t, h, nodeB, "shared-name", "/opt/otherix/pools/B")
	_ = seedPoolDirect(t, h, nodeB, "node-b-only", "/opt/otherix/pools/B-only")

	// nodeA's heartbeat: must see only the A-rooted shared-name entry.
	resp := postHeartbeatExpect200(t, ctx, h, nodeA.Name, heartbeatBodyWithPools("amd64", nil))
	if len(resp.DeclaredPools) != 1 {
		t.Fatalf("nodeA declared_pools count = %d, want 1; got %+v", len(resp.DeclaredPools), resp.DeclaredPools)
	}
	if resp.DeclaredPools[0].Path != "/opt/otherix/pools/A" {
		t.Errorf("nodeA pool path = %q, want /opt/otherix/pools/A — the B-rooted twin must not leak", resp.DeclaredPools[0].Path)
	}
}
