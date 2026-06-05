// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
)

func TestHandler_Health(t *testing.T) {
	m := Start(t, Options{})
	resp := mustGet(t, m.URL()+"/v1/health")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got agentapi.HealthResponse
	decode(t, resp, &got)
	if got.Status != "ok" {
		t.Errorf("status = %q, want ok", got.Status)
	}
}

func TestHandler_InfoIncludesPreloadedState(t *testing.T) {
	m := Start(t, Options{NodeName: "test-node"})
	if err := m.SetCapability("kvm_available", true); err != nil {
		t.Fatalf("SetCapability: %v", err)
	}
	if err := m.SetCapability("cpu_cores_total", 8); err != nil {
		t.Fatalf("SetCapability: %v", err)
	}
	m.AddFirmware(Firmware{
		Name:         "OVMF_CODE",
		Architecture: "amd64",
		Type:         "uefi",
		CodePath:     "/usr/share/OVMF/OVMF_CODE.fd",
	})

	resp := mustGet(t, m.URL()+"/v1/info")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var info agentapi.NodeInfo
	decode(t, resp, &info)
	if info.NodeID != m.NodeID() {
		t.Errorf("NodeId = %v, want %v", info.NodeID, m.NodeID())
	}
	if info.Hostname != "test-node" {
		t.Errorf("Hostname = %q, want test-node", info.Hostname)
	}
	if !info.KvmAvailable {
		t.Errorf("KvmAvailable = false, want true")
	}
	if info.CPUCoresTotal != 8 {
		t.Errorf("CpuCoresTotal = %d, want 8", info.CPUCoresTotal)
	}
	if len(info.Firmwares) != 1 {
		t.Fatalf("len(Firmwares) = %d, want 1", len(info.Firmwares))
	}
	if info.Firmwares[0].Name != "OVMF_CODE" {
		t.Errorf("Firmwares[0].Name = %q, want OVMF_CODE", info.Firmwares[0].Name)
	}
}

func TestHandler_StoragePoolsList(t *testing.T) {
	m := Start(t, Options{})
	id := uuid.New()
	name := "default"
	m.AddStoragePool(StoragePool{
		ID:   id,
		Name: name,
		Type: "local_dir",
		Path: "/opt/otherix/pools/default",
	})
	if err := m.SetPoolCapacity(name, 100<<30, 50<<30); err != nil {
		t.Fatalf("SetPoolCapacity: %v", err)
	}

	resp := mustGet(t, m.URL()+"/v1/storage-pools")
	var list agentapi.StoragePoolList
	decode(t, resp, &list)
	if len(list.Data) != 1 {
		t.Fatalf("len(Data) = %d, want 1", len(list.Data))
	}
	if list.Data[0].ID != id {
		t.Errorf("pool id = %v, want %v", list.Data[0].ID, id)
	}
	if list.Meta.NextCursor != nil {
		t.Errorf("next_cursor = %v, want nil for single-page result", list.Meta.NextCursor)
	}
}

func TestHandler_StoragePoolsGet_NotFound(t *testing.T) {
	m := Start(t, Options{})
	resp := mustGet(t, m.URL()+"/v1/storage-pools/"+uuid.NewString())
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var env agentapi.Error
	decode(t, resp, &env)
	if env.Error.Code != "not_found" {
		t.Errorf("error.code = %q, want not_found", env.Error.Code)
	}
}

func TestHandler_TasksGet_AlwaysNotFound(t *testing.T) {
	m := Start(t, Options{})
	resp := mustGet(t, m.URL()+"/v1/tasks/"+uuid.NewString())
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (tasks endpoint always 404s)", resp.StatusCode)
	}
}

func TestHandler_StubReturnsNotImplementedEnvelope(t *testing.T) {
	m := Start(t, Options{})
	resp, err := http.Get(m.URL() + "/v1/networks") //nolint:gosec // test URL.
	if err != nil {
		t.Fatalf("GET /v1/networks: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
	var env agentapi.Error
	decode(t, resp, &env)
	if env.Error.Code != "internal" {
		t.Errorf("error.code = %q, want internal", env.Error.Code)
	}
	wantMsg := "not implemented in mock-agent"
	if env.Error.Message != wantMsg {
		t.Errorf("error.message = %q, want %q", env.Error.Message, wantMsg)
	}
}

func TestHandler_RequestRecording(t *testing.T) {
	m := Start(t, Options{})
	mustGet(t, m.URL()+"/v1/health")
	mustGet(t, m.URL()+"/v1/info")

	got := m.ReceivedRequests()
	if len(got) != 2 {
		t.Fatalf("len(ReceivedRequests) = %d, want 2", len(got))
	}
	wantOps := []string{"health.check", "info.get"}
	for i, want := range wantOps {
		if got[i].OperationID != want {
			t.Errorf("requests[%d].OperationID = %q, want %q", i, got[i].OperationID, want)
		}
		if got[i].Method != http.MethodGet {
			t.Errorf("requests[%d].Method = %q, want GET", i, got[i].Method)
		}
	}
}

func TestHandler_BodyCapture(t *testing.T) {
	m := Start(t, Options{})
	body := strings.NewReader(`{"hello":"world"}`)
	resp, err := http.Post(m.URL()+"/v1/networks", "application/json", body)
	if err != nil {
		t.Fatalf("POST /v1/networks: %v", err)
	}
	defer resp.Body.Close()

	got := m.ReceivedRequests()
	if len(got) != 1 {
		t.Fatalf("len(ReceivedRequests) = %d, want 1", len(got))
	}
	if string(got[0].Body) != `{"hello":"world"}` {
		t.Errorf("Body = %q, want JSON payload", string(got[0].Body))
	}
	if got[0].Truncated {
		t.Errorf("Truncated = true, want false for small body")
	}
}

// decode reads the response body, closes it, and json-unmarshals into
// target.
func decode(t *testing.T, resp *http.Response, target any) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(body, target); err != nil {
		t.Fatalf("unmarshal %T: %v\nbody=%s", target, err, body)
	}
}
