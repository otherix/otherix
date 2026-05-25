// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// TestClusterDefaultPool_FullLifecycle exercises the
// cluster_settings singleton through its HTTP surface:
//
//  1. GET returns 404 default_pool_not_set when no default is
//     configured (the seed verticalSlice never sets one).
//  2. PUT with an unknown name returns 400 pool_not_found.
//  3. PUT with the seeded pool's name succeeds and surfaces the
//     canonical-case spelling on the response.
//  4. GET now returns 200 with the persisted name.
//  5. DELETE clears the reference; subsequent GET is 404 again.
//
// One verticalSlice carries the whole flow — every test step
// re-uses the same control plane (faster setup than per-step
// teardown, and matches the operator's real lifecycle).
func TestClusterDefaultPool_FullLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v := newVerticalSlice(t, ctx, fastAgentClientCfg())

	// Step 1: unset by default.
	if status, body := v.clusterGetDefaultPool(t, ctx); status != http.StatusNotFound {
		t.Fatalf("initial GET status = %d, body = %s, want 404", status, body)
	} else if !jsonContains(body, "default_pool_not_set") {
		t.Errorf("initial GET body = %s, want code default_pool_not_set", body)
	}

	// Step 2: unknown name → 400 pool_not_found.
	if status, body := v.clusterSetDefaultPool(t, ctx, "no-such-pool"); status != http.StatusBadRequest {
		t.Fatalf("PUT unknown-name status = %d, body = %s, want 400", status, body)
	} else if !jsonContains(body, "pool_not_found") {
		t.Errorf("PUT unknown-name body = %s, want code pool_not_found", body)
	}

	// Step 3: known name → 200 + canonical echo.
	status, body := v.clusterSetDefaultPool(t, ctx, v.pool.Name)
	if status != http.StatusOK {
		t.Fatalf("PUT happy status = %d, body = %s, want 200", status, body)
	}
	var setResp struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &setResp); err != nil {
		t.Fatalf("decode PUT response: %v", err)
	}
	if setResp.Name != v.pool.Name {
		t.Errorf("PUT response name = %q, want %q", setResp.Name, v.pool.Name)
	}

	// Step 4: GET surfaces persisted name.
	status, body = v.clusterGetDefaultPool(t, ctx)
	if status != http.StatusOK {
		t.Fatalf("post-set GET status = %d, body = %s, want 200", status, body)
	}
	var getResp struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &getResp); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if getResp.Name != v.pool.Name {
		t.Errorf("GET name = %q, want %q", getResp.Name, v.pool.Name)
	}

	// Step 5: DELETE clears.
	if status, body := v.clusterClearDefaultPool(t, ctx); status != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, body = %s, want 204", status, body)
	}
	if status, _ := v.clusterGetDefaultPool(t, ctx); status != http.StatusNotFound {
		t.Fatalf("post-clear GET status = %d, want 404", status)
	}
}

// TestVMCreate_FallsBackToClusterDefault verifies the fallback
// behaviour: a VM-create request that omits `pool` succeeds when a
// cluster default-pool is set, and fails 400 default_pool_not_set
// otherwise.
func TestVMCreate_FallsBackToClusterDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-default", 0xb2, "private")

	// No default configured → 400 default_pool_not_set.
	status, body := v.createVMRaw(t, ctx, vmCreateBody{
		Name:     "fallback-vm-" + uuid.NewString()[:8],
		Template: tpl.Name,
		VCPUs:    2,
		MemoryMB: 1024,
		// Pool intentionally omitted.
	}, v.adminToken, "")
	if status != http.StatusBadRequest {
		t.Fatalf("create without --pool status = %d, body = %s, want 400", status, body)
	}
	if !jsonContains(body, "default_pool_not_set") {
		t.Errorf("create without --pool body = %s, want code default_pool_not_set", body)
	}

	// Set the seeded pool as default — same name resolution.
	if status, body := v.clusterSetDefaultPool(t, ctx, v.pool.Name); status != http.StatusOK {
		t.Fatalf("PUT default pool status = %d, body = %s, want 200", status, body)
	}

	// Now the same request should succeed and dispatch to the seeded
	// pool's owning node.
	taskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     "fallback-ok-" + uuid.NewString()[:8],
		Template: tpl.Name,
		VCPUs:    2,
		MemoryMB: 1024,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)

	row, err := v.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status == store.TaskStatusFailed {
		t.Fatalf("task failed: %s", string(row.Error))
	}
}

// clusterGetDefaultPool / clusterSetDefaultPool / clusterClearDefaultPool
// are local thin wrappers around v.cpServer.URL + /v1/cluster/default-pool.
// Kept here rather than in vm_vertical_setup_test.go because they are
// specific to this file's flow — the rest of the suite does not exercise
// /v1/cluster/* directly.
func (v *verticalSlice) clusterGetDefaultPool(t *testing.T, ctx context.Context) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cpServer.URL+"/v1/cluster/default-pool", nil)
	if err != nil {
		t.Fatalf("GET default-pool req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+v.adminToken)
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("GET default-pool: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read GET default-pool body: %v", err)
	}
	return resp.StatusCode, body
}

func (v *verticalSlice) clusterSetDefaultPool(t *testing.T, ctx context.Context, name string) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		t.Fatalf("marshal set default-pool: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, v.cpServer.URL+"/v1/cluster/default-pool", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("PUT default-pool req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.adminToken)
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("PUT default-pool: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read PUT default-pool body: %v", err)
	}
	return resp.StatusCode, body
}

func (v *verticalSlice) clusterClearDefaultPool(t *testing.T, ctx context.Context) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, v.cpServer.URL+"/v1/cluster/default-pool", nil)
	if err != nil {
		t.Fatalf("DELETE default-pool req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+v.adminToken)
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE default-pool: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read DELETE default-pool body: %v", err)
	}
	return resp.StatusCode, body
}
