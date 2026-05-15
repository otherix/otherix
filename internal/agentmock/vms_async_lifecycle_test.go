// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentmock"
)

// TestVmsAsyncLifecycle_StartStopRebootPoweroff_HappyPath exercises
// the L2 async surface end-to-end: create → stop → start → reboot →
// poweroff. Each op returns 202 + AsyncTaskAccepted; the inventory
// transition materialises lazily on subsequent TasksGet calls.
func TestVmsAsyncLifecycle_StartStopRebootPoweroff_HappyPath(t *testing.T) {
	m := startMock(t)
	vmID := uuid.New()
	poolID := "test-pool"

	createTaskID := postVMCreate(t, m, validVMBody(vmID, poolID))
	if task := pollUntilTerminal(t, m, createTaskID, 2*time.Second); string(task.Status) != "success" {
		t.Fatalf("create status = %q, want success", task.Status)
	}

	// Default success transitions per op:
	//   stop:     running → stopped
	//   start:    stopped → running
	//   reboot:   running → running
	//   poweroff: running → stopped
	cases := []struct {
		op   string
		want string
	}{
		{"stop", "stopped"},
		{"start", "running"},
		{"reboot", "running"},
		{"poweroff", "stopped"},
	}
	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			taskID := postAsyncLifecycle(t, m, "demo", tc.op)
			if task := pollUntilTerminal(t, m, taskID, 2*time.Second); string(task.Status) != "success" {
				t.Fatalf("%s status = %q, want success", tc.op, task.Status)
			}
			v, ok := m.GetStoredVM("demo")
			if !ok {
				t.Fatalf("vm absent after %s", tc.op)
			}
			if v.Status != tc.want {
				t.Errorf("status after %s = %q, want %q", tc.op, v.Status, tc.want)
			}
		})
	}
}

// TestVmsAsyncLifecycle_StopTimeout_FailureLeavesRunning verifies
// that а queued stop-timeout failure surfaces as task.status="failed"
// и leaves the inventory entry в running (no transition on failure
// per Area 4-IV — manual intervention only).
func TestVmsAsyncLifecycle_StopTimeout_FailureLeavesRunning(t *testing.T) {
	m := startMock(t)
	vmID := uuid.New()
	poolID := "test-pool"

	createTaskID := postVMCreate(t, m, validVMBody(vmID, poolID))
	if task := pollUntilTerminal(t, m, createTaskID, 2*time.Second); string(task.Status) != "success" {
		t.Fatalf("create status = %q, want success", task.Status)
	}

	m.AddVMLifecycleResult("demo", "stop", agentmock.VMLifecycleResult{
		Status: "failed",
		Error: &agentmock.ErrorEnvelope{
			Code:    "stop_timeout",
			Message: "guest did not honour ACPI shutdown",
		},
	})

	taskID := postAsyncLifecycle(t, m, "demo", "stop")
	task := pollUntilTerminal(t, m, taskID, 2*time.Second)
	if string(task.Status) != "failed" {
		t.Errorf("stop status = %q, want failed", task.Status)
	}
	if task.Error == nil || task.Error.Code == nil || string(*task.Error.Code) != "stop_timeout" {
		t.Errorf("stop error.code = %+v, want stop_timeout", task.Error)
	}
	if v, _ := m.GetStoredVM("demo"); v.Status != "running" {
		t.Errorf("inventory status after stop-fail = %q, want running (Area 4-IV: manual intervention)", v.Status)
	}
}

// TestVmsAsyncLifecycle_NotFound — the mock returns 202 always (the
// real agent surfaces 404 only when there is no inventory entry, but
// the mock's task surface is name-keyed and registers а task row
// regardless). The materialise hook is the differentiator: it skips
// when the inventory is empty, so the task succeeds without staging
// а phantom inventory entry. Verifies the contract for а repeated
// op against а non-existent VM does not corrupt state.
func TestVmsAsyncLifecycle_NotFound(t *testing.T) {
	m := startMock(t)
	for _, op := range []string{"start", "stop", "poweroff", "reboot"} {
		taskID := postAsyncLifecycle(t, m, "no-such-vm", op)
		if task := pollUntilTerminal(t, m, taskID, 2*time.Second); string(task.Status) != "success" {
			t.Errorf("%s no-such status = %q, want success (mock layer no-op)", op, task.Status)
		}
		if _, ok := m.GetStoredVM("no-such-vm"); ok {
			t.Errorf("%s materialised а phantom inventory entry for no-such-vm", op)
		}
	}
}

func postAsyncLifecycle(t *testing.T, m mockURL, vmName, op string) uuid.UUID {
	t.Helper()
	url := m.URL() + "/v1/vms/" + vmName + "/" + op
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("%s %s status = %d, want 202", op, vmName, resp.StatusCode)
	}
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var body struct {
		TaskID uuid.UUID `json:"task_id"`
	}
	if err := json.Unmarshal(buf, &body); err != nil {
		t.Fatalf("decode async accepted body: %v (%s)", err, buf)
	}
	if body.TaskID == uuid.Nil {
		t.Fatalf("%s returned zero task id; body=%s", op, buf)
	}
	return body.TaskID
}
