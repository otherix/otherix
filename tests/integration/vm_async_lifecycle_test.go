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
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/store"
)

// TestVMAsyncLifecycle_StartStopRebootPoweroff_HappyPath drives the
// full L2 async pipeline end-to-end against the mock-agent: create →
// stop (running → stopped) → start (stopped → running) → reboot
// (running → running) → poweroff (running → stopped) → start → delete.
// Each L2 op is enqueued through the river worker (CP-side), the
// agent emits the matching task, and the CP projects success +
// desired_phase write. Asserts: 202 from the handler, terminal
// success from the task row, vms.desired_phase updates per spec
// (start → running, stop / poweroff → stopped, reboot unchanged),
// and agent inventory state.
func TestVMAsyncLifecycle_StartStopRebootPoweroff_HappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-async-lifecycle", 0xc0, "private")

	vmName := "async-vm-" + uuid.NewString()[:8]
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

	// stop: running → stopped, desired_phase → stopped, runtime → stopped.
	runAsyncOp(t, ctx, v, vmID, "stop", store.VmDesiredPhaseStopped, store.VmPhaseStopped, "stopped")
	// start: stopped → running, desired_phase → running, runtime → running.
	runAsyncOp(t, ctx, v, vmID, "start", store.VmDesiredPhaseRunning, store.VmPhaseRunning, "running")
	// reboot: running → running, desired_phase unchanged, runtime → running.
	runAsyncOp(t, ctx, v, vmID, "reboot", store.VmDesiredPhaseRunning, store.VmPhaseRunning, "running")
	// poweroff: running → stopped, desired_phase → stopped, runtime → stopped.
	runAsyncOp(t, ctx, v, vmID, "poweroff", store.VmDesiredPhaseStopped, store.VmPhaseStopped, "stopped")
}

// TestVMAsyncLifecycle_StopTimeout_TaskFailsLeavesRunning verifies
// that an agent-side stop_timeout failure surfaces as a terminal-
// failed CP task with the agent code preserved verbatim, and that the
// CP does NOT write desired_phase to stopped on failure (the InTx
// rolls back). Mock-agent stages stop-timeout via
// AddVMLifecycleResult.
func TestVMAsyncLifecycle_StopTimeout_TaskFailsLeavesRunning(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-async-fail", 0xc1, "private")

	vmName := "stop-timeout-vm-" + uuid.NewString()[:8]
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

	v.mock.AddVMLifecycleResult(vmName, "stop", agentmock.VMLifecycleResult{
		Status: "failed",
		Error: &agentmock.ErrorEnvelope{
			Code:    "stop_timeout",
			Message: "guest did not honour ACPI shutdown within 60s",
		},
	})

	taskID := postAsyncLifecycle(t, ctx, v, vmID, "stop", "")
	awaitTaskTerminal(t, ctx, v, taskID, 15*time.Second)
	row, err := v.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusFailed {
		t.Errorf("task status = %q, want failed", row.Status)
	}
	if !jsonContains(row.Error, `stop_timeout`) {
		t.Errorf("task error envelope missing stop_timeout code: %s", row.Error)
	}
	vmRow, err := v.store.Queries().GetVMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("GetVMByID: %v", err)
	}
	if vmRow.DesiredPhase != store.VmDesiredPhaseRunning {
		t.Errorf("desired_phase after stop failure = %q, want running (InTx rolled back)", vmRow.DesiredPhase)
	}
}

// TestVMAsyncLifecycle_CrossUserNotFound — developer attempts to
// start admin's VM. PermVMLifecycle scope=own + cross-user maps to
// 404 (no leak) per the project's 403-vs-404 rule. Same shape as
// L1 TestVMLifecycle_CrossUserNotFound.
func TestVMAsyncLifecycle_CrossUserNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	v := newVerticalSlice(t, ctx, fastAgentClientCfg())
	tpl := seedTemplateForVM(t, ctx, v, v.adminID, "vm-async-rbac", 0xc2, "private")

	createTaskID, _ := v.createVM(t, ctx, vmCreateBody{
		Name:     "rbac-async-vm-" + uuid.NewString()[:8],
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

	for _, op := range []string{"start", "stop", "poweroff", "reboot"} {
		status, body := postAsyncLifecycleRaw(t, ctx, v, vmID, op, devToken)
		if status != http.StatusNotFound {
			t.Errorf("cross-user %s status = %d, body = %s, want 404 (no leak)", op, status, body)
		}
	}
}

// runAsyncOp POSTs the supplied op, awaits the task terminal, and
// asserts the post-state matches expectations: terminal-success, CP
// desired_phase, CP vm_runtime.phase (observed-state mirror), and
// mock-agent inventory phase.
func runAsyncOp(t *testing.T, ctx context.Context, v *verticalSlice, vmID uuid.UUID, op string, wantDesired store.VMDesiredPhase, wantRuntime store.VMPhase, wantAgentStatus string) {
	t.Helper()
	taskID := postAsyncLifecycle(t, ctx, v, vmID, op, "")
	awaitTaskTerminal(t, ctx, v, taskID, 15*time.Second)
	row, err := v.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("[%s] GetTask: %v", op, err)
	}
	if row.Status != store.TaskStatusSuccess {
		t.Fatalf("[%s] task status = %q error = %s, want success", op, row.Status, row.Error)
	}
	vmRow, err := v.store.Queries().GetVMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("[%s] GetVMByID: %v", op, err)
	}
	if vmRow.DesiredPhase != wantDesired {
		t.Errorf("[%s] desired_phase = %q, want %q", op, vmRow.DesiredPhase, wantDesired)
	}
	rt, err := v.store.Queries().GetVMRuntime(ctx, vmID)
	if err != nil {
		t.Fatalf("[%s] GetVMRuntime: %v", op, err)
	}
	if rt.Phase != wantRuntime {
		t.Errorf("[%s] vm_runtime.phase = %q, want %q", op, rt.Phase, wantRuntime)
	}
	if vmFromMock, ok := v.mock.StoredVM(vmRow.Name); !ok {
		t.Errorf("[%s] mock-agent inventory missing entry for %s", op, vmRow.Name)
	} else if vmFromMock.Status != wantAgentStatus {
		t.Errorf("[%s] mock-agent inventory status = %q, want %q", op, vmFromMock.Status, wantAgentStatus)
	}
}

// postAsyncLifecycle returns the task_id from a 202 response, fatal
// on a non-202 status.
func postAsyncLifecycle(t *testing.T, ctx context.Context, v *verticalSlice, vmID uuid.UUID, op, token string) uuid.UUID {
	t.Helper()
	status, body := postAsyncLifecycleRaw(t, ctx, v, vmID, op, token)
	if status != http.StatusAccepted {
		t.Fatalf("%s status = %d, body = %s, want 202", op, status, body)
	}
	var accepted struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(body, &accepted); err != nil {
		t.Fatalf("decode AsyncTaskAccepted: %v (%s)", err, body)
	}
	id, err := uuid.Parse(accepted.TaskID)
	if err != nil {
		t.Fatalf("parse task_id %q: %v", accepted.TaskID, err)
	}
	return id
}

// postAsyncLifecycleRaw issues the POST and returns (status, body)
// without status validation — used by negative-path tests checking 404
// / 409 / 401.
func postAsyncLifecycleRaw(t *testing.T, ctx context.Context, v *verticalSlice, vmID uuid.UUID, op, token string) (int, []byte) {
	t.Helper()
	row, err := v.store.Queries().GetVMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("resolve vm name: %v", err)
	}
	url := v.cpServer.URL + "/v1/vms/" + row.Name + "/" + op
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("new %s request: %v", op, err)
	}
	if token == "" {
		token = v.adminToken
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", op, err)
	}
	return resp.StatusCode, body
}

// awaitTaskTerminal polls the task row until it reaches a terminal
// status (success / failed / cancelled), or the deadline fires.
// Uses the store directly rather than the GET /v1/tasks/{id} surface
// — equivalent for our purposes, and saves the auth dance for a tail
// poll. Sleeps 50ms between polls so the test bench does not burn
// CPU.
func awaitTaskTerminal(t *testing.T, ctx context.Context, v *verticalSlice, taskID uuid.UUID, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		row, err := v.store.Queries().GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		switch row.Status {
		case store.TaskStatusSuccess, store.TaskStatusFailed, store.TaskStatusCancelled:
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach terminal within %v", taskID, deadline)
}
