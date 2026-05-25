// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/agentmock"
)

// vmCreateBody mirrors the wire shape the mock decodes — the
// simplified Iteration 1 agent createRequest. Tests construct it
// directly to avoid coupling to internal package types. The wire
// field is `pool` (was `pool_id`) for consistency with the CP edge.
type vmCreateBody struct {
	UUID             string `json:"uuid"`
	Name             string `json:"name"`
	VCPUs            int    `json:"vcpus"`
	MemoryMB         int    `json:"memory_mb"`
	Pool             string `json:"pool"`
	TemplateChecksum string `json:"template_checksum"`
}

func validVMBody(vmID uuid.UUID, poolName string) vmCreateBody {
	return validNamedVMBody(vmID, "demo", poolName)
}

// validNamedVMBody is the named variant — needed by tests that
// register multiple VMs against one mock (post Pre-L1 Path D the
// mock's storedVMs is keyed by name, so re-using `"demo"` would
// fold subsequent creates into idempotent re-creates rather than
// staging independent entries).
func validNamedVMBody(vmID uuid.UUID, name, poolName string) vmCreateBody {
	return vmCreateBody{
		UUID:             vmID.String(),
		Name:             name,
		VCPUs:            2,
		MemoryMB:         2048,
		Pool:             poolName,
		TemplateChecksum: strings.Repeat("a", 64),
	}
}

func postVMCreate(t *testing.T, m *agentmock.Mock, body vmCreateBody) uuid.UUID {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(m.URL()+"/v1/vms", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post vms: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("vms.create status = %d, want 202", resp.StatusCode)
	}
	var accepted agentapi.AsyncTaskAccepted
	if err := json.NewDecoder(resp.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode AsyncTaskAccepted: %v", err)
	}
	if accepted.TaskID == uuid.Nil {
		t.Fatal("accepted.TaskID is zero")
	}
	return accepted.TaskID
}

func TestVmsCreate_DefaultSuccessMaterialises(t *testing.T) {
	m := startMock(t)
	vmID := uuid.New()
	poolID := "test-pool"

	taskID := postVMCreate(t, m, validVMBody(vmID, poolID))
	task := pollUntilTerminal(t, m, taskID, 2*time.Second)
	if string(task.Status) != "success" {
		t.Fatalf("status = %q, want success", task.Status)
	}

	v, ok := m.StoredVM("demo")
	if !ok {
		t.Fatalf("StoredVM(%q) = false; want staged after terminal-success", "demo")
	}
	if v.ID != vmID {
		t.Errorf("stored vm.ID = %s, want %s", v.ID, vmID)
	}
	if v.Name != "demo" || v.VCPUs != 2 || v.MemoryMB != 2048 || v.PoolName != poolID {
		t.Errorf("stored vm = %+v, want name=demo vcpus=2 memory=2048 pool=%s", v, poolID)
	}
	if v.Architecture != "amd64" {
		t.Errorf("architecture = %q, want amd64 (mock node default)", v.Architecture)
	}
	if v.Status != "running" {
		t.Errorf("status = %q, want running (default)", v.Status)
	}
}

func TestVmsCreate_QueuedFailure(t *testing.T) {
	m := startMock(t)
	vmID := uuid.New()
	poolID := "test-pool"

	m.AddVMCreateResult("demo", agentmock.VMCreateResult{
		Status: "failed",
		Error: &agentmock.ErrorEnvelope{
			Code:    "internal",
			Message: "synthetic create failure",
		},
		Delay: 30 * time.Millisecond,
	})

	taskID := postVMCreate(t, m, validVMBody(vmID, poolID))
	task := pollUntilTerminal(t, m, taskID, 2*time.Second)
	if string(task.Status) != "failed" {
		t.Fatalf("status = %q, want failed", task.Status)
	}
	if task.Error == nil || task.Error.Code == nil || *task.Error.Code != "internal" {
		t.Fatalf("error = %+v, want code=internal", task.Error)
	}
	if _, present := m.StoredVM("demo"); present {
		t.Errorf("VM materialised on failed create")
	}
}

func TestVmsCreate_IdempotentReCreate(t *testing.T) {
	m := startMock(t)
	vmID := uuid.New()
	poolID := "test-pool"

	// First create — default success.
	taskID1 := postVMCreate(t, m, validVMBody(vmID, poolID))
	if task := pollUntilTerminal(t, m, taskID1, 2*time.Second); string(task.Status) != "success" {
		t.Fatalf("first status = %q, want success", task.Status)
	}

	// Queue a failure that must NOT fire because the VM is already
	// staged (idempotent re-create).
	m.AddVMCreateResult("demo", agentmock.VMCreateResult{
		Status: "failed",
		Error:  &agentmock.ErrorEnvelope{Code: "internal", Message: "should not fire"},
		Delay:  20 * time.Millisecond,
	})

	taskID2 := postVMCreate(t, m, validVMBody(vmID, poolID))
	task := pollUntilTerminal(t, m, taskID2, 2*time.Second)
	if string(task.Status) != "success" {
		t.Fatalf("re-create status = %q, want success (idempotent — failure queue must be preserved)", task.Status)
	}
}

func TestVmsCreate_ValidationFailures(t *testing.T) {
	m := startMock(t)

	// Missing uuid.
	resp, err := http.Post(m.URL()+"/v1/vms", "application/json",
		strings.NewReader(`{"name":"x","vcpus":1,"memory_mb":1024,"pool":"`+uuid.NewString()+`"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing uuid status = %d, want 400", resp.StatusCode)
	}

	// Invalid pool_id.
	resp, err = http.Post(m.URL()+"/v1/vms", "application/json",
		strings.NewReader(`{"uuid":"`+uuid.NewString()+`","name":"x","vcpus":1,"memory_mb":1024,"pool_id":"not-a-uuid"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid pool status = %d, want 400", resp.StatusCode)
	}
}

func TestVmsGet_HappyPath(t *testing.T) {
	m := startMock(t)
	vmID := uuid.New()
	poolID := "test-pool"

	taskID := postVMCreate(t, m, validVMBody(vmID, poolID))
	pollUntilTerminal(t, m, taskID, 2*time.Second)

	resp, err := http.Get(m.URL() + "/v1/vms/demo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["id"] != vmID.String() {
		t.Errorf("id = %v, want %s", got["id"], vmID)
	}
	if got["name"] != "demo" {
		t.Errorf("name = %v, want demo", got["name"])
	}
	if got["pool"] != poolID {
		t.Errorf("pool = %v, want %s", got["pool"], poolID)
	}
}

func TestVmsGet_NotFound(t *testing.T) {
	m := startMock(t)

	resp, err := http.Get(m.URL() + "/v1/vms/no-such-vm")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestVmsList_HappyPath(t *testing.T) {
	m := startMock(t)
	poolID := "test-pool"

	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	names := []string{"vm-list-a", "vm-list-b", "vm-list-c"}
	for i, id := range ids {
		taskID := postVMCreate(t, m, validNamedVMBody(id, names[i], poolID))
		pollUntilTerminal(t, m, taskID, 2*time.Second)
	}

	resp, err := http.Get(m.URL() + "/v1/vms")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Data []map[string]any `json:"data"`
		Meta map[string]any   `json:"meta"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Data) != 3 {
		t.Fatalf("data len = %d, want 3", len(body.Data))
	}
	gotIDs := make([]string, 0, 3)
	for _, item := range body.Data {
		gotIDs = append(gotIDs, item["id"].(string))
	}
	wantIDs := []string{ids[0].String(), ids[1].String(), ids[2].String()}
	sort.Strings(gotIDs)
	sort.Strings(wantIDs)
	for i := range gotIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Errorf("ids[%d] = %s, want %s", i, gotIDs[i], wantIDs[i])
		}
	}
	if _, ok := body.Meta["next_cursor"]; !ok {
		t.Errorf("meta.next_cursor missing")
	}
}

func TestVmsDelete_HappyPath(t *testing.T) {
	m := startMock(t)
	vmID := uuid.New()
	poolID := "test-pool"

	createTaskID := postVMCreate(t, m, validVMBody(vmID, poolID))
	pollUntilTerminal(t, m, createTaskID, 2*time.Second)

	url := m.URL() + "/v1/vms/demo"
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var accepted agentapi.AsyncTaskAccepted
	if err := json.NewDecoder(resp.Body).Decode(&accepted); err != nil {
		t.Fatalf("decode: %v", err)
	}
	task := pollUntilTerminal(t, m, accepted.TaskID, 2*time.Second)
	if string(task.Status) != "success" {
		t.Fatalf("delete status = %q, want success", task.Status)
	}
	if _, present := m.StoredVM("demo"); present {
		t.Errorf("VM still present after successful delete")
	}
}

func TestVmsDelete_MissingIsAccepted(t *testing.T) {
	m := startMock(t)

	url := m.URL() + "/v1/vms/no-such-vm"
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202 (delete-of-missing is idempotent)", resp.StatusCode)
	}
}

func TestVmsDelete_QueuedFailurePreservesVM(t *testing.T) {
	m := startMock(t)
	vmID := uuid.New()
	poolID := "test-pool"

	// Create the VM.
	createTaskID := postVMCreate(t, m, validVMBody(vmID, poolID))
	pollUntilTerminal(t, m, createTaskID, 2*time.Second)

	// Queue a failure for the upcoming delete.
	m.AddVMDeleteResult("demo", agentmock.VMDeleteResult{
		Status: "failed",
		Error:  &agentmock.ErrorEnvelope{Code: "internal", Message: "synthetic delete failure"},
		Delay:  20 * time.Millisecond,
	})

	url := m.URL() + "/v1/vms/demo"
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	var accepted agentapi.AsyncTaskAccepted
	_ = json.NewDecoder(resp.Body).Decode(&accepted)
	_ = resp.Body.Close()

	task := pollUntilTerminal(t, m, accepted.TaskID, 2*time.Second)
	if string(task.Status) != "failed" {
		t.Fatalf("delete status = %q, want failed", task.Status)
	}
	if _, present := m.StoredVM("demo"); !present {
		t.Errorf("VM removed despite failed delete")
	}
}
