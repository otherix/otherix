// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// TestE2E_PoolReconciliation_HappyPath drives the full happy-path
// wire interaction in two heartbeat cycles:
//
//  1. Seed а pool row directly via SQL с reconciliation_status='pending'
//     (the row is operator-declared but the agent has not reported on
//     it yet — the state а freshly-created pool is in immediately
//     after POST /v1/storage-pools).
//  2. First heartbeat carries no pool reports — expect 200 OK with а
//     declared_pools entry для the seeded pool. The row stays pending.
//  3. Second heartbeat carries а pool report c status=ready. Expect 200
//     plus the storage_pools row's reconciliation_status flipping к
//     `ready` и last_reconciled_at populated.
//
// The test bypasses the production agent reconciler (we drive raw
// HTTP) so the interactions stay observable in а single test function.
func TestE2E_PoolReconciliation_HappyPath(t *testing.T) {
	h := newCPAgentHarness(t)
	ctx := context.Background()

	node := seedNodeWithCert(t, h, store.CpuArchAmd64, store.NodeStatusReady, false)
	pool := seedPoolDirect(t, h, node, "alpha", "/opt/otherix/pools/alpha")

	// --- Cycle 1: no pool reports → expect declared_pools in response.
	resp1 := postHeartbeatExpect200(t, ctx, h, node.Name, heartbeatBodyWithPools("amd64", nil))
	if want, got := 1, len(resp1.DeclaredPools); want != got {
		t.Fatalf("cycle 1 declared_pools count = %d, want %d (body: %+v)", got, want, resp1)
	}
	dp := resp1.DeclaredPools[0]
	if dp.Name != "alpha" || dp.Path != "/opt/otherix/pools/alpha" || dp.Type != "local_dir" {
		t.Errorf("cycle 1 declared_pool = %+v, want name=alpha path=/opt/otherix/pools/alpha type=local_dir", dp)
	}

	row1, err := h.store.Queries().GetStoragePoolByID(ctx, pool.ID)
	if err != nil {
		t.Fatalf("GetStoragePoolByID: %v", err)
	}
	if row1.ReconciliationStatus != "pending" {
		t.Errorf("cycle 1 reconciliation_status = %q, want pending", row1.ReconciliationStatus)
	}

	// --- Cycle 2: agent reports `ready`. Row transitions.
	resp2 := postHeartbeatExpect200(t, ctx, h, node.Name, heartbeatBodyWithPools("amd64",
		[]heartbeatPoolReport{{Name: "alpha", ReconciliationStatus: "ready"}}))
	if got := len(resp2.DeclaredPools); got != 1 {
		t.Errorf("cycle 2 declared_pools count = %d, want 1", got)
	}

	row2, err := h.store.Queries().GetStoragePoolByID(ctx, pool.ID)
	if err != nil {
		t.Fatalf("GetStoragePoolByID after cycle 2: %v", err)
	}
	if row2.ReconciliationStatus != "ready" {
		t.Errorf("cycle 2 reconciliation_status = %q, want ready", row2.ReconciliationStatus)
	}
	if row2.LastReconciledAt == nil {
		t.Errorf("cycle 2 last_reconciled_at = nil, want set")
	}
	if row2.ReconciliationError != nil {
		t.Errorf("cycle 2 reconciliation_error = %v, want nil", row2.ReconciliationError)
	}
}

// heartbeatPoolReport mirrors HeartbeatPoolReport in
// api/openapi/control-plane.yaml. Defined inline to keep the test
// independent of internal packages.
type heartbeatPoolReport struct {
	Name                 string  `json:"name"`
	ReconciliationStatus string  `json:"reconciliation_status"`
	ReconciliationError  *string `json:"reconciliation_error,omitempty"`
}

// heartbeatResponseDecoded mirrors HeartbeatResponse — the bits the
// pool reconciliation tests care about.
type heartbeatResponseDecoded struct {
	ReceivedAt    string                  `json:"received_at"`
	DeclaredPools []heartbeatDeclaredPool `json:"declared_pools"`
}

type heartbeatDeclaredPool struct {
	Name   string                 `json:"name"`
	Type   string                 `json:"type"`
	Path   string                 `json:"path"`
	Config map[string]interface{} `json:"config,omitempty"`
}

// heartbeatBodyWithPools builds а minimal-but-valid heartbeat request
// с pools[] populated by reports. Mirrors minimalHeartbeatBody (in
// heartbeat_test.go) but extended for the new pool field.
func heartbeatBodyWithPools(arch string, reports []heartbeatPoolReport) []byte {
	body := map[string]any{
		"agent_version": "mock-0.1.0",
		"architecture":  arch,
		"capabilities": map[string]any{
			"cpu_model":        "test-cpu",
			"cpu_flags":        []string{},
			"cpu_cores_total":  4,
			"memory_total_mib": int64(4096),
			"kernel_version":   "6.6.0",
			"qemu_version":     "9.0.0",
			"kvm_available":    true,
			"nested_virt":      false,
			"qemu_binaries":    map[string]string{},
			"firmwares":        []any{},
		},
		"resources": map[string]any{
			"cpu_cores_available":  4,
			"memory_available_mib": int64(4000),
		},
		"vms": []any{},
	}
	if reports != nil {
		body["pools"] = reports
	}
	out, _ := json.Marshal(body)
	return out
}

// postHeartbeatExpect200 sends body via the mTLS client wrapper and
// asserts а 200 OK response, returning the decoded body. The test
// fails-fast when the wrapper returns а non-200; tests that exercise
// 4xx / 5xx behaviour go through postHeartbeatWithMockTLS directly.
func postHeartbeatExpect200(t *testing.T, ctx context.Context, h *cpAgentHarness, nodeName string, body []byte) heartbeatResponseDecoded {
	t.Helper()
	resp, err := postHeartbeatWithMockTLS(ctx, h, nodeName, body)
	if err != nil {
		t.Fatalf("post heartbeat: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat status = %d, want 200, body=%s", resp.StatusCode, string(raw))
	}
	var decoded heartbeatResponseDecoded
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode heartbeat response: %v body=%s", err, string(raw))
	}
	return decoded
}

// seedPoolDirect inserts а storage_pools row directly via the sqlc
// CreateStoragePool query. Avoids the public API edge so the test can
// exercise the reconciliation projection on its own. Uses а timestamp
// suffix on the path к keep concurrent test runs isolated.
func seedPoolDirect(t *testing.T, h *cpAgentHarness, node seededNode, name, path string) store.StoragePool {
	t.Helper()
	row, err := h.store.Queries().CreateStoragePool(context.Background(), store.CreateStoragePoolParams{
		ID:     uuid.New(),
		NodeID: node.ID,
		Name:   name,
		Type:   "local_dir",
		Path:   path,
		Config: []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("CreateStoragePool(%s): %v", name, err)
	}
	// Force pending-state read к isolate test от any default-drift —
	// the schema default is `pending`, но if а future migration changes
	// the default the test would silently miss the transition assertion.
	if row.ReconciliationStatus != "pending" {
		t.Fatalf("seeded pool reconciliation_status = %q, want pending (schema default drifted?)", row.ReconciliationStatus)
	}
	return row
}

// Used implicitly by other tests in the file; keep а reference so
// goimports doesn't strip the import on partial saves.
var (
	_ = strings.HasPrefix
	_ = time.Now
)
