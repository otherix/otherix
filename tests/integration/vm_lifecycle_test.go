// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestVMLifecycle_PauseResumeReset_HappyPath drives the full L1 sync
// pipeline end-to-end against the mock-agent: create → pause →
// resume → reset → delete. Each sync op is а direct CP→agent HTTP
// call (no river — the L1 path is synchronous per spec). The test
// asserts both the wire response (200 + projected status) and the
// post-call vm_runtime row.
func TestVMLifecycle_PauseResumeReset_HappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-lifecycle", 0xae, "private")

	vmName := "lifecycle-vm-" + uuid.NewString()[:8]
	createTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     vmName,
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    1,
		MemoryMB: 512,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)
	createRow, _ := v.store.Queries().GetTask(ctx, createTaskID)
	vmID := extractVMIDFromTask(t, createRow)

	// Pause running → paused.
	status, body := v.postVMLifecycleRequest(t, ctx, vmID, "pause", "")
	if status != http.StatusOK {
		t.Fatalf("pause status = %d, body = %s, want 200", status, body)
	}
	if !jsonContains(body, `"status":"paused"`) {
		t.Errorf("pause body missing status=paused: %s", body)
	}
	if rt, err := v.store.Queries().GetVMRuntime(ctx, vmID); err != nil {
		t.Fatalf("GetVMRuntime after pause: %v", err)
	} else if string(rt.Phase) != "paused" {
		t.Errorf("vm_runtime.phase after pause = %q, want paused", rt.Phase)
	}

	// Resume paused → running.
	status, body = v.postVMLifecycleRequest(t, ctx, vmID, "resume", "")
	if status != http.StatusOK {
		t.Fatalf("resume status = %d, body = %s, want 200", status, body)
	}
	if !jsonContains(body, `"status":"running"`) {
		t.Errorf("resume body missing status=running: %s", body)
	}
	if rt, err := v.store.Queries().GetVMRuntime(ctx, vmID); err != nil {
		t.Fatalf("GetVMRuntime after resume: %v", err)
	} else if string(rt.Phase) != "running" {
		t.Errorf("vm_runtime.phase after resume = %q, want running", rt.Phase)
	}

	// Reset running stays running (runtime identity preserved).
	status, body = v.postVMLifecycleRequest(t, ctx, vmID, "reset", "")
	if status != http.StatusOK {
		t.Fatalf("reset status = %d, body = %s, want 200", status, body)
	}
	if !jsonContains(body, `"status":"running"`) {
		t.Errorf("reset body missing status=running: %s", body)
	}
}

// TestVMLifecycle_PauseWhenStopped_Conflict — pausing а VM that the
// mock-agent reports as `stopped` must return 409 from the CP. Agent
// returns 409; CP propagates the envelope verbatim through
// writeLifecycleError.
func TestVMLifecycle_PauseWhenStopped_Conflict(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-lifecycle-c", 0xaf, "private")

	vmName := "lifecycle-conflict-vm-" + uuid.NewString()[:8]
	createTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     vmName,
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    1,
		MemoryMB: 512,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)
	createRow, _ := v.store.Queries().GetTask(ctx, createTaskID)
	vmID := extractVMIDFromTask(t, createRow)

	// Walk the agent's inventory entry into a non-running state by
	// pausing first; subsequent pause must reject с 409.
	if status, _ := v.postVMLifecycleRequest(t, ctx, vmID, "pause", ""); status != http.StatusOK {
		t.Fatalf("first pause status = %d, want 200", status)
	}
	status, body := v.postVMLifecycleRequest(t, ctx, vmID, "pause", "")
	if status != http.StatusConflict {
		t.Errorf("second pause status = %d, body = %s, want 409 (already paused)", status, body)
	}
}

// TestVMLifecycle_CrossUserNotFound — developer attempts К pause
// admin's VM. PermVMLifecycle scope=own + cross-user maps к 404 (no
// leak) per the project's 403-vs-404 rule.
func TestVMLifecycle_CrossUserNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-lifecycle-r", 0xb0, "private")

	createTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     "rbac-pause-vm-" + uuid.NewString()[:8],
		Template: tpl.Name,
		Pool:     v.pool.ID.String(),
		VCPUs:    1,
		MemoryMB: 512,
	}, "")
	v.awaitVMCreateEvent(t, 15*time.Second)
	createRow, _ := v.store.Queries().GetTask(ctx, createTaskID)
	vmID := extractVMIDFromTask(t, createRow)

	_, devEmail, devPW := seedUser(t, ctx, v.store, "developer")
	devToken := loginUser(t, v.cpServer.URL, devEmail, devPW)

	status, body := v.postVMLifecycleRequest(t, ctx, vmID, "pause", devToken)
	if status != http.StatusNotFound {
		t.Errorf("cross-user pause status = %d, body = %s, want 404 (no leak)", status, body)
	}
}
