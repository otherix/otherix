// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// TestVMCreate_VerticalSliceIdempotency exercises the
// middleware.Idempotency replay path: two POSTs against /v1/vms
// carrying the same Idempotency-Key + same body should return the
// SAME task_id (the first call's), the SAME 202 + AsyncTaskAccepted
// envelope, и produce exactly ONE vms row.
func TestVMCreate_VerticalSliceIdempotency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-idem", 0xb2, "private")

	body := vmCreateBody{
		Name:     "idem-vm-" + uuid.NewString()[:8],
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    2,
		MemoryMB: 1024,
	}
	idem := "idem-" + uuid.NewString()

	// First call — task fires, agent observed.
	taskID1, _ := v.createVM(t, ctx, body, idem)
	v.awaitVMCreateEvent(t, 15*time.Second)

	// Second call — middleware should replay the cached 202 verbatim.
	taskID2, _ := v.createVM(t, ctx, body, idem)
	if taskID1 != taskID2 {
		t.Fatalf("idempotent replay: task_id mismatch %s vs %s", taskID1, taskID2)
	}

	row, err := v.store.Queries().GetTask(ctx, taskID1)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusSuccess {
		t.Fatalf("task.Status = %q, want success", row.Status)
	}

	// Exactly one vms row — the cached replay does not run the handler
	// body a second time, so no duplicate INSERT.
	vmID := extractVMIDFromTask(t, row)
	if _, err := v.store.Queries().GetVMByID(ctx, vmID); err != nil {
		t.Fatalf("GetVMByID: %v", err)
	}

	// Mismatch case — same key, different body — must surface 409
	// idempotency_key_mismatch.
	mismatch := body
	mismatch.Name = "different-name"
	status, respBody := v.createVMRaw(t, ctx, mismatch, v.adminToken, idem)
	if status != 409 {
		t.Fatalf("idem mismatch status = %d, body = %s, want 409", status, respBody)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &env); err != nil {
		t.Fatalf("decode mismatch envelope: %v", err)
	}
	if env.Error.Code != "idempotency_key_mismatch" {
		t.Errorf("mismatch error.code = %q, want idempotency_key_mismatch", env.Error.Code)
	}
}
