// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik
//
// Vertical-slice e2e: agent-side terminal failure.

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
	"github.com/otherix/otherix/internal/store"
)

// TestStoragePoolsScan_VerticalSlice_AgentFailure exercises the
// agent-reports-failure branch of the saga: the mock-agent
// transitions its task to terminal `failed` with an error envelope,
// the CP worker observes that on its next poll, projects the
// failure into tasks.error via the worker's existing fail() path,
// and storage_pools is left untouched (no projection on failure).
func TestStoragePoolsScan_VerticalSlice_AgentFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	v.mock.AddPoolScanResult(v.pool.Name, agentmock.PoolScanResult{
		Status: "failed",
		Error: &agentmock.ErrorEnvelope{
			Code:    "internal",
			Message: "disk fault",
		},
		Delay: 30 * time.Millisecond,
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
	if row.Result != nil {
		t.Errorf("task.Result = %s, want nil on failure", string(row.Result))
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
	// CP worker wraps every executor error into the scan_failed
	// envelope (Step 7 pass-through mapping); the originating
	// agent code/message land in the message via fmt.Errorf
	// "agent task failed: <code>: <message>".
	if envelope.Code != "scan_failed" {
		t.Errorf("envelope.code = %q, want scan_failed", envelope.Code)
	}
	if !strings.Contains(envelope.Message, "internal") || !strings.Contains(envelope.Message, "disk fault") {
		t.Errorf("envelope.message = %q, want substrings 'internal' and 'disk fault'", envelope.Message)
	}

	pool, err := v.store.Queries().GetStoragePoolByID(ctx, v.pool.ID)
	if err != nil {
		t.Fatalf("GetStoragePoolByID: %v", err)
	}
	if pool.CapacityBytes != nil || pool.AvailableBytes != nil || pool.ReportedAt != nil {
		t.Errorf("storage_pools touched on failure path: cap=%v avail=%v reported=%v",
			pool.CapacityBytes, pool.AvailableBytes, pool.ReportedAt)
	}
}
