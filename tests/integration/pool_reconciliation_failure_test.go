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

// TestE2E_PoolReconciliation_AgentReportsFailed confirms that the
// heartbeat handler projects а failed pool report onto the
// storage_pools row: reconciliation_status='failed' + error message
// stored verbatim. The agent retries forever; the CP
// surfaces the latest reported error for operator visibility (no
// retry-count cap).
func TestE2E_PoolReconciliation_AgentReportsFailed(t *testing.T) {
	h := newCPAgentHarness(t)
	ctx := context.Background()

	node := seedNodeWithCert(t, h, store.CpuArchAmd64, store.NodeStatusReady, false)
	pool := seedPoolDirect(t, h, node, "doomed", "/tmp/forbidden")

	errMsg := "mkdir /tmp/forbidden: permission denied"
	resp := postHeartbeatExpect200(t, ctx, h, node.Name, heartbeatBodyWithPools("amd64",
		[]heartbeatPoolReport{{
			Name:                 "doomed",
			ReconciliationStatus: "failed",
			ReconciliationError:  &errMsg,
		}}))

	if got := len(resp.DeclaredPools); got != 1 {
		t.Errorf("declared_pools count = %d, want 1", got)
	}

	row, err := h.store.Queries().GetStoragePoolByID(ctx, pool.ID)
	if err != nil {
		t.Fatalf("GetStoragePoolByID: %v", err)
	}
	if row.ReconciliationStatus != "failed" {
		t.Errorf("reconciliation_status = %q, want failed", row.ReconciliationStatus)
	}
	if row.ReconciliationError == nil || *row.ReconciliationError != errMsg {
		t.Errorf("reconciliation_error = %v, want %q", row.ReconciliationError, errMsg)
	}
	if row.LastReconciledAt == nil {
		t.Errorf("last_reconciled_at = nil, want set on every report")
	}
}

// TestE2E_PoolReconciliation_RecoverFromFailed confirms the row flips
// back к `ready` когда the agent reports success after а prior
// `failed` cycle. Closes the loop on eventual-consistency: nothing
// special needs to happen on the CP — а later heartbeat with а
// happy report just overwrites the column.
func TestE2E_PoolReconciliation_RecoverFromFailed(t *testing.T) {
	h := newCPAgentHarness(t)
	ctx := context.Background()

	node := seedNodeWithCert(t, h, store.CpuArchAmd64, store.NodeStatusReady, false)
	pool := seedPoolDirect(t, h, node, "flaky", "/opt/otherix/pools/flaky")

	errMsg := "transient io error"
	_ = postHeartbeatExpect200(t, ctx, h, node.Name, heartbeatBodyWithPools("amd64",
		[]heartbeatPoolReport{{Name: "flaky", ReconciliationStatus: "failed", ReconciliationError: &errMsg}}))

	row1, _ := h.store.Queries().GetStoragePoolByID(ctx, pool.ID)
	if row1.ReconciliationStatus != "failed" {
		t.Fatalf("intermediate status = %q, want failed", row1.ReconciliationStatus)
	}

	_ = postHeartbeatExpect200(t, ctx, h, node.Name, heartbeatBodyWithPools("amd64",
		[]heartbeatPoolReport{{Name: "flaky", ReconciliationStatus: "ready"}}))

	row2, _ := h.store.Queries().GetStoragePoolByID(ctx, pool.ID)
	if row2.ReconciliationStatus != "ready" {
		t.Errorf("final status = %q, want ready", row2.ReconciliationStatus)
	}
	// The agent reports nil error on success — handler stores NULL.
	if row2.ReconciliationError != nil {
		t.Errorf("recovered reconciliation_error = %v, want nil", row2.ReconciliationError)
	}
}
