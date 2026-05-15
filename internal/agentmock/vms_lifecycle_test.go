// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestVmsLifecycle_PauseResumeReset_HappyPath exercises the mock-agent's
// L1 sync surface end-to-end: create → pause → resume → reset. The
// stored AgentVM tracks status across the transitions; reset stays
// `running` because the QMP `system_reset` preserves runtime identity.
func TestVmsLifecycle_PauseResumeReset_HappyPath(t *testing.T) {
	m := startMock(t)
	vmID := uuid.New()
	poolID := "test-pool"

	taskID := postVMCreate(t, m, validVMBody(vmID, poolID))
	if task := pollUntilTerminal(t, m, taskID, 2*time.Second); string(task.Status) != "success" {
		t.Fatalf("create status = %q, want success", task.Status)
	}

	pause := postLifecycle(t, m, "demo", "pause")
	if pause["status"] != "paused" {
		t.Errorf("pause status = %v, want paused", pause["status"])
	}
	if v, _ := m.GetStoredVM("demo"); v.Status != "paused" {
		t.Errorf("stored status after pause = %q, want paused", v.Status)
	}

	resume := postLifecycle(t, m, "demo", "resume")
	if resume["status"] != "running" {
		t.Errorf("resume status = %v, want running", resume["status"])
	}
	if v, _ := m.GetStoredVM("demo"); v.Status != "running" {
		t.Errorf("stored status after resume = %q, want running", v.Status)
	}

	reset := postLifecycle(t, m, "demo", "reset")
	if reset["status"] != "running" {
		t.Errorf("reset status = %v, want running (identity preserved)", reset["status"])
	}
}

// TestVmsLifecycle_PauseWhenPaused_Conflict — а second pause against
// the same VM returns 409 from the mock-agent. Mirrors the real-agent
// state-precondition envelope so CP-side tests can rely on the same
// shape.
func TestVmsLifecycle_PauseWhenPaused_Conflict(t *testing.T) {
	m := startMock(t)
	vmID := uuid.New()
	poolID := "test-pool"

	taskID := postVMCreate(t, m, validVMBody(vmID, poolID))
	if task := pollUntilTerminal(t, m, taskID, 2*time.Second); string(task.Status) != "success" {
		t.Fatalf("create status = %q, want success", task.Status)
	}
	_ = postLifecycle(t, m, "demo", "pause")

	status, _ := postLifecycleRaw(t, m, "demo", "pause")
	if status != http.StatusConflict {
		t.Errorf("second pause status = %d, want 409", status)
	}
}

// TestVmsLifecycle_NotFound — the mock returns 404 for а name not в
// the inventory regardless of action.
func TestVmsLifecycle_NotFound(t *testing.T) {
	m := startMock(t)
	for _, action := range []string{"pause", "resume", "reset"} {
		status, _ := postLifecycleRaw(t, m, "no-such-vm", action)
		if status != http.StatusNotFound {
			t.Errorf("%s no-such status = %d, want 404", action, status)
		}
	}
}

func postLifecycle(t *testing.T, m mockURL, vmName, action string) map[string]any {
	t.Helper()
	status, body := postLifecycleRaw(t, m, vmName, action)
	if status != http.StatusOK {
		t.Fatalf("%s %s status = %d, body = %s, want 200", action, vmName, status, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s body: %v", action, err)
	}
	return out
}

func postLifecycleRaw(t *testing.T, m mockURL, vmName, action string) (int, []byte) {
	t.Helper()
	url := m.URL() + "/v1/vms/" + vmName + "/" + action
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", action, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := make([]byte, 0, 1024)
	buf := make([]byte, 256)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	return resp.StatusCode, body
}

// mockURL is the minimal URL-bearing surface postLifecycle needs.
// startMock returns *agentmock.Mock, which satisfies this.
type mockURL interface {
	URL() string
}
