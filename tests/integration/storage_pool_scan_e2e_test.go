// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik
//
// Vertical-slice e2e: happy path and idempotency replay.

//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/store"
)

// TestStoragePoolsScan_VerticalSlice_HappyPath drives the full
// vertical-slice chain: real CP HTTP server → real river →
// agentScanExecutor (real *agentclient.Client, mTLS) → real
// mock-agent functional scan → projection back into storage_pools.
// Every layer except Postgres is the production type.
func TestStoragePoolsScan_VerticalSlice_HappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	v.mock.AddPoolScanResult(v.pool.Name, agentmock.PoolScanResult{
		Status:         "success",
		CapacityBytes:  1 << 40,
		AvailableBytes: 1 << 39,
		Delay:          30 * time.Millisecond,
	})

	_, taskID := v.postScan(t, ctx, "")
	ev := v.awaitScanEvent(t, 15*time.Second)
	if ev.Kind != "job_completed" {
		t.Fatalf("scan event Kind = %q, want job_completed", ev.Kind)
	}

	row, err := v.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusSuccess {
		t.Errorf("task.Status = %q, want success", row.Status)
	}
	var result struct {
		CapacityBytes  int64     `json:"capacity_bytes"`
		AvailableBytes int64     `json:"available_bytes"`
		ReportedAt     time.Time `json:"reported_at"`
	}
	if err := json.Unmarshal(row.Result, &result); err != nil {
		t.Fatalf("unmarshal Result: %v", err)
	}
	if result.CapacityBytes != 1<<40 {
		t.Errorf("result.capacity_bytes = %d, want 1<<40", result.CapacityBytes)
	}
	if result.AvailableBytes != 1<<39 {
		t.Errorf("result.available_bytes = %d, want 1<<39", result.AvailableBytes)
	}
	if result.ReportedAt.IsZero() {
		t.Errorf("result.reported_at is zero")
	}

	pool, err := v.store.Queries().GetStoragePoolByID(ctx, v.pool.ID)
	if err != nil {
		t.Fatalf("GetStoragePoolByID: %v", err)
	}
	if pool.CapacityBytes == nil || *pool.CapacityBytes != 1<<40 {
		t.Errorf("pool.CapacityBytes = %v, want 1<<40", pool.CapacityBytes)
	}
	if pool.AvailableBytes == nil || *pool.AvailableBytes != 1<<39 {
		t.Errorf("pool.AvailableBytes = %v, want 1<<39", pool.AvailableBytes)
	}

	if got := v.agentScanCallCount(); got != 1 {
		t.Errorf("agentmock storagePools.scan calls = %d, want 1", got)
	}

	// Subsequent GET /v1/tasks/{id} via the public HTTP surface
	// returns the projected Task — assert the contract round-trips.
	taskURL := v.cpServer.URL + "/v1/tasks/" + taskID.String()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, taskURL, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+v.adminToken)
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("tasks.get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tasks.get status = %d, want 200", resp.StatusCode)
	}
	var task struct {
		Status string          `json:"status"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	if task.Status != "success" {
		t.Errorf("public Task.status = %q, want success", task.Status)
	}
}

// TestStoragePoolsScan_VerticalSlice_IdempotencyReplay verifies the
// idempotency contract end to end: a repeat POST with
// the same Idempotency-Key replays the cached 202 body verbatim and
// MUST NOT trigger a second agent-side scan.
func TestStoragePoolsScan_VerticalSlice_IdempotencyReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	v.mock.AddPoolScanResult(v.pool.Name, agentmock.PoolScanResult{
		Status:         "success",
		CapacityBytes:  4242,
		AvailableBytes: 4141,
		Delay:          30 * time.Millisecond,
	})

	idemKey := "vertical-idem-" + uuid.NewString()

	// First call enqueues; capture the 202 body.
	body1, taskID1 := v.postScan(t, ctx, idemKey)

	// Second call (within the 24h idempotency window) replays the
	// cached body verbatim: byte-identical task_id and status. The
	// middleware records 2xx responses by status-code-agnostic
	// design; the assertion below is the empirical confirmation.
	req2 := v.scanRequest(t, ctx, []byte(`{}`), idemKey)
	resp2, err := v.cpServer.Client().Do(req2)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("second scan status = %d, want 202", resp2.StatusCode)
	}
	var body2 bytes.Buffer
	if _, err := body2.ReadFrom(resp2.Body); err != nil {
		t.Fatalf("read second body: %v", err)
	}
	if !bytes.Equal(body1, body2.Bytes()) {
		t.Errorf("second body differs from first (idempotency replay broken)\nfirst:  %s\nsecond: %s", body1, body2.String())
	}

	// Wait for the first (and only) scan to land.
	v.awaitScanEvent(t, 15*time.Second)

	// Mock-agent observed exactly one POST: the second CP request
	// was a middleware replay, never re-enqueued through the worker.
	if got := v.agentScanCallCount(); got != 1 {
		t.Errorf("agentmock storagePools.scan calls = %d, want 1 (idempotency replay must not double-call)", got)
	}

	row, err := v.store.Queries().GetTask(ctx, taskID1)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusSuccess {
		t.Errorf("task.Status = %q, want success", row.Status)
	}

	// Idempotency-key body mismatch sanity check: a third call
	// with the same key but a divergent body must surface 409
	// idempotency_key_mismatch. Confirms the cache
	// keys on (user, method, path, body-hash) end to end.
	req3 := v.scanRequest(t, ctx, []byte(`{"divergent":true}`), idemKey)
	resp3, err := v.cpServer.Client().Do(req3)
	if err != nil {
		t.Fatalf("third scan: %v", err)
	}
	defer func() { _ = resp3.Body.Close() }()
	if resp3.StatusCode != http.StatusConflict {
		t.Errorf("body-mismatch status = %d, want 409", resp3.StatusCode)
	}
	var conflict struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp3.Body).Decode(&conflict); err != nil {
		t.Fatalf("decode 409: %v", err)
	}
	if conflict.Error.Code != "idempotency_key_mismatch" {
		t.Errorf("error.code = %q, want idempotency_key_mismatch", conflict.Error.Code)
	}
}
