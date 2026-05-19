// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik
//
// Vertical-slice e2e: tasks.cancel against a running task.

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/store"
)

// TestTasks_CancelRunning_Returns409 exercises pending-only
// cancellation: a task already advanced to running by
// the worker cannot be cancelled — `tasks.cancel` returns
// 409 with `details.code = task_not_cancellable`. The test seeds a
// long-running scan (5s agent delay), waits for the worker to push
// the task into status=running, then issues cancel via the public
// HTTP surface.
//
// The worker's in-flight polling loop is interrupted at test cleanup
// by riverClient.Stop, which propagates ctx.Done() into PollTask;
// the agent task is left dangling on the mock side (acceptable —
// future running-cancellation work will propagate ctx into a real
// agent-cancel).
func TestTasks_CancelRunning_Returns409(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())

	// Long agent delay so the worker is still polling when the test
	// fires its cancel. 5s is large relative to the 200ms cancel
	// roundtrip but well under the test ctx 60s budget.
	v.mock.AddPoolScanResult(v.pool.Name, agentmock.PoolScanResult{
		Status:        "success",
		CapacityBytes: 1,
		Delay:         5 * time.Second,
	})

	_, taskID := v.postScan(t, ctx, "")

	// Wait for the worker to advance the row from pending → running.
	v.pollTaskUntil(t, ctx, taskID, func(row store.Task) bool {
		return row.Status == store.TaskStatusRunning
	}, 5*time.Second)

	cancelURL := v.cpServer.URL + "/v1/tasks/" + taskID.String() + "/cancel"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cancelURL, http.NoBody)
	if err != nil {
		t.Fatalf("new cancel request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+v.adminToken)
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("cancel POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("cancel status = %d, want 409", resp.StatusCode)
	}
	var envelope struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode 409: %v", err)
	}
	if envelope.Error.Code != "conflict" {
		t.Errorf("error.code = %q, want conflict", envelope.Error.Code)
	}
	if got, _ := envelope.Error.Details["code"].(string); got != "task_not_cancellable" {
		t.Errorf("error.details.code = %q, want task_not_cancellable", got)
	}

	// Sanity: row stayed in running (cancel rolled back the
	// transactional river-side JobCancelTx alongside).
	row, err := v.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusRunning {
		t.Errorf("post-cancel status = %q, want running (cancel must not transition)", row.Status)
	}
}
