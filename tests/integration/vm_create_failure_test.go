// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/store"
)

// TestVMCreate_VerticalSliceAgentFailure injects a 500 +
// qemu_spawn_failed envelope at the agent on POST /v1/vms. The
// executor's PostVMCreate surfaces this as *AgentError; the worker's
// classifyVMError preserves the code; the task surface goes terminal-
// failed with `error.code = "qemu_spawn_failed"`.
//
// The vms row stays — operators inspect the `failed` task to decide
// remediation, and the eventual user-driven delete walks через the
// regular vm.delete path. derived_vm_count was never incremented
// (increment happens только in the success projectAndFinalize InTx).
func TestVMCreate_VerticalSliceAgentFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-fail", 0xc3, "private")

	v.mock.InjectError("vms.create", agentmock.InjectedError{
		Status:  http.StatusInternalServerError,
		Code:    "qemu_spawn_failed",
		Message: "kvm not available и tcg fallback disabled",
	})

	taskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     "fail-vm-" + uuid.NewString()[:8],
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    2,
		MemoryMB: 1024,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)

	row, err := v.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusFailed {
		t.Fatalf("task.Status = %q, want failed", row.Status)
	}
	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(row.Error, &env); err != nil {
		t.Fatalf("decode task.error: %v", err)
	}
	if env.Code != "qemu_spawn_failed" {
		t.Errorf("task.error.code = %q, want qemu_spawn_failed (passthrough)", env.Code)
	}

	// derived_vm_count untouched — increment lives inside the success
	// InTx, не on the failure path.
	tplAfter, err := v.store.Queries().GetTemplate(ctx, tpl.ID)
	if err != nil {
		t.Fatalf("GetTemplate (after): %v", err)
	}
	if tplAfter.DerivedVmCount != 0 {
		t.Errorf("template.derived_vm_count = %d, want 0 (failed create must not increment)", tplAfter.DerivedVmCount)
	}

	// vms row was inserted at handler time — the failure path leaves
	// it for operator inspection. (Task surface carries the failure
	// reason; no implicit row cleanup.)
	vmID := extractVMIDFromTask(t, row)
	vmRow, err := v.store.Queries().GetVMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("GetVMByID after failed create: %v", err)
	}
	if vmRow.DeletedAt != nil {
		t.Errorf("vm.deleted_at = %v after failed create; want nil (no implicit cleanup)", vmRow.DeletedAt)
	}
	// vm_runtime row was not upserted (worker's projectCreateSuccess
	// only fires on success).
	if _, err := v.store.Queries().GetVMRuntime(ctx, vmID); err == nil {
		t.Errorf("vm_runtime present after failed create; want absent")
	}
}

// TestVMCreate_VerticalSliceValidation covers the API-edge validation
// branches не hit by the unit tests' http-recorder fakes. The
// integration path is end-to-end: real router, real auth, real
// envelope encoder.
func TestVMCreate_VerticalSliceValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-val", 0xd4, "private")

	// vcpus too small.
	status, body := v.createVMRaw(t, ctx, vmCreateBody{
		Name:     "x",
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    0,
		MemoryMB: 1024,
	}, v.adminToken, "")
	if status != http.StatusBadRequest {
		t.Errorf("vcpus=0 status = %d, want 400 (body=%s)", status, body)
	}
	if !jsonContains(body, `"validation_failed"`) {
		t.Errorf("vcpus=0 envelope missing validation_failed: %s", body)
	}

	// memory_mb too large.
	status, body = v.createVMRaw(t, ctx, vmCreateBody{
		Name:     "x",
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    2,
		MemoryMB: 1 << 30,
	}, v.adminToken, "")
	if status != http.StatusBadRequest {
		t.Errorf("oversized memory status = %d, want 400 (body=%s)", status, body)
	}

	// Pool that doesn't exist → 404 not_found.
	status, body = v.createVMRaw(t, ctx, vmCreateBody{
		Name:     "x",
		Template: tpl.Name,
		Pool:     uuid.NewString(),
		VCPUs:    2,
		MemoryMB: 1024,
	}, v.adminToken, "")
	if status != http.StatusNotFound {
		t.Errorf("missing-pool status = %d, want 404 (body=%s)", status, body)
	}
}
