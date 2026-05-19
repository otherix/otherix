// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik
//
// Vertical-slice e2e: CP polling-budget exhaustion.

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/store"
)

// TestStoragePoolsScan_VerticalSlice_PollingBudgetExceeded drives
// the CP-side timeout branch: the agentclient's PollTask budget
// elapses before the agent task reaches a terminal state. Worker
// finalizes the CP task as failed with the *TimeoutError surfacing
// in the envelope message; storage_pools stays untouched.
//
// The test pins the agent-client config to an aggressive 200ms
// budget and the mock-agent's delay to 5s — the gap is wide enough
// that the cumulative-poll deadline always fires regardless of
// scheduler jitter on loaded CI.
func TestStoragePoolsScan_VerticalSlice_PollingBudgetExceeded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg := config.AgentClientConfig{
		Enabled:         true,
		Timeout:         200 * time.Millisecond,
		PollInterval:    10 * time.Millisecond,
		PollMaxInterval: 100 * time.Millisecond,
	}
	v := newVerticalSlice(t, ctx, cfg)

	v.mock.AddPoolScanResult(v.pool.Name, agentmock.PoolScanResult{
		Status:        "success",
		CapacityBytes: 1,
		Delay:         5 * time.Second, // far beyond CP budget.
	})

	_, taskID := v.postScan(t, ctx, "")
	v.awaitScanEvent(t, 15*time.Second)

	row, err := v.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusFailed {
		t.Errorf("task.Status = %q, want failed", row.Status)
	}
	if row.Error == nil {
		t.Fatal("task.Error = nil, want envelope")
	}
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(row.Error, &envelope); err != nil {
		t.Fatalf("unmarshal Error: %v", err)
	}
	if envelope.Code != "scan_failed" {
		t.Errorf("envelope.code = %q, want scan_failed", envelope.Code)
	}
	// agentclient's *TimeoutError renders "agentclient: poll budget
	// 200ms exceeded for task <id>" — Step 6's wire shape. Worker's
	// fail() wraps it as "poll task: <TimeoutError>", and the
	// envelope message carries the chain. Match on the stable
	// substring "poll budget" rather than the full string so test
	// stays robust to log-format tweaks.
	if !strings.Contains(envelope.Message, "poll budget") {
		t.Errorf("envelope.message = %q, want substring 'poll budget' (timeout signal)", envelope.Message)
	}

	pool, err := v.store.Queries().GetStoragePoolByID(ctx, v.pool.ID)
	if err != nil {
		t.Fatalf("GetStoragePoolByID: %v", err)
	}
	if pool.CapacityBytes != nil || pool.AvailableBytes != nil || pool.ReportedAt != nil {
		t.Errorf("storage_pools touched on timeout path: cap=%v avail=%v reported=%v",
			pool.CapacityBytes, pool.AvailableBytes, pool.ReportedAt)
	}
}
