// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
)

// newClientAutoDir creates a fresh tempdir, drops cert material in
// it via writeMaterial, and constructs an agentclient. Used by tests
// that don't need access to the underlying dir.
func newClientAutoDir(t *testing.T) *agentclient.Client {
	t.Helper()
	dir := writeMaterial(t)
	return newClient(t, dir)
}

func validVMCreateRequest() agentclient.VMCreateRequest {
	return agentclient.VMCreateRequest{
		UUID:           uuid.New(),
		Name:           "test-vm",
		VCPUs:          2,
		MemoryMib:      2048,
		Pool:           "test-pool",
		ImageURL:       "https://example.test/ubuntu.img",
		ExpectedSHA256: strings.Repeat("a", 64),
	}
}

func TestPostVMCreate_HappyPath(t *testing.T) {
	t.Parallel()

	wantTaskID := uuid.New()
	wantIdem := "idem-" + uuid.NewString()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Idempotency-Key"); got != wantIdem {
			t.Errorf("Idempotency-Key = %q, want %q", got, wantIdem)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		jsonWrite(t, w, http.StatusAccepted, map[string]any{
			"task_id": wantTaskID,
			"status":  "pending",
			"links":   map[string]any{"self": "/v1/tasks/" + wantTaskID.String()},
		})
	})

	_, baseURL := startAgentTLSServer(t, mux)

	gotTaskID, err := newClientAutoDir(t).PostVMCreate(
		context.Background(), baseURL, wantIdem, validVMCreateRequest())
	if err != nil {
		t.Fatalf("PostVMCreate: %v", err)
	}
	if gotTaskID != wantTaskID {
		t.Errorf("task_id = %s, want %s", gotTaskID, wantTaskID)
	}
}

func TestPostVMCreate_EncodesNics(t *testing.T) {
	t.Parallel()

	nicID := uuid.New()
	taskID := uuid.New()
	type wireNIC struct {
		ID          string `json:"id"`
		Bridge      string `json:"bridge"`
		MAC         string `json:"mac"`
		Model       string `json:"model"`
		MTU         int    `json:"mtu"`
		DeviceOrder int    `json:"device_order"`
	}
	type wireBody struct {
		Nics []wireNIC `json:"nics"`
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms", func(w http.ResponseWriter, r *http.Request) {
		var body wireBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if len(body.Nics) != 1 {
			t.Fatalf("nics = %d, want 1", len(body.Nics))
		}
		got := body.Nics[0]
		want := wireNIC{ID: nicID.String(), Bridge: "br0", MAC: "52:54:00:12:34:56", Model: "virtio", MTU: 1500, DeviceOrder: 0}
		if got != want {
			t.Errorf("nic = %+v, want %+v", got, want)
		}
		jsonWrite(t, w, http.StatusAccepted, map[string]any{
			"task_id": taskID,
			"status":  "pending",
			"links":   map[string]any{"self": "/v1/tasks/" + taskID.String()},
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	req := validVMCreateRequest()
	req.Nics = []agentclient.VMCreateNIC{{
		ID: nicID, Bridge: "br0", MAC: "52:54:00:12:34:56", Model: "virtio", MTU: 1500, DeviceOrder: 0,
	}}
	if _, err := newClientAutoDir(t).PostVMCreate(context.Background(), baseURL, "", req); err != nil {
		t.Fatalf("PostVMCreate: %v", err)
	}
}

func TestPostVMCreate_AgentError5xx(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms", func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(t, w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"code": "internal", "message": "agent overloaded"},
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	_, err := newClientAutoDir(t).PostVMCreate(
		context.Background(), baseURL, "", validVMCreateRequest())
	if err == nil {
		t.Fatalf("expected error on 503")
	}

	var ae *agentclient.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v; want *AgentError", err)
	}
	if ae.Status != http.StatusServiceUnavailable {
		t.Errorf("Status = %d, want 503", ae.Status)
	}
	if !ae.IsRetryable() {
		t.Errorf("IsRetryable() = false, want true for 5xx")
	}
}

func TestPostVMCreate_Conflict409(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms", func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(t, w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"code":    "conflict",
				"message": "vm already exists",
				"details": map[string]any{"vm_id": "x"},
			},
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	_, err := newClientAutoDir(t).PostVMCreate(
		context.Background(), baseURL, "", validVMCreateRequest())
	if err == nil {
		t.Fatalf("expected error on 409")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
		t.Fatalf("err = %v; want AgentError 409", err)
	}
	if ae.IsRetryable() {
		t.Errorf("4xx should not be retryable")
	}
}

func TestPostVMCreate_MalformedAccepted(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("not json"))
	})
	_, baseURL := startAgentTLSServer(t, mux)

	_, err := newClientAutoDir(t).PostVMCreate(
		context.Background(), baseURL, "", validVMCreateRequest())
	if err == nil {
		t.Fatalf("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode AsyncTaskAccepted") {
		t.Errorf("err = %v; want decode-error wrapper", err)
	}
}

func TestGetVM_HappyPath(t *testing.T) {
	t.Parallel()

	vmID := uuid.New()
	vmName := "demo"
	want := agentclient.AgentVM{
		ID:           vmID,
		Name:         vmName,
		VCPUs:        4,
		MemoryMib:    4096,
		Pool:         "test-pool",
		Architecture: "arm64",
		Status:       "running",
		CreatedAt:    time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 5, 9, 12, 5, 0, 0, time.UTC),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms/"+vmName, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		jsonWrite(t, w, http.StatusOK, map[string]any{
			"id":           want.ID,
			"name":         want.Name,
			"vcpus":        want.VCPUs,
			"memory_mib":   want.MemoryMib,
			"pool":         want.Pool,
			"architecture": want.Architecture,
			"status":       want.Status,
			"created_at":   want.CreatedAt.Format(time.RFC3339),
			"updated_at":   want.UpdatedAt.Format(time.RFC3339),
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	got, err := newClientAutoDir(t).VM(context.Background(), baseURL, vmName)
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if got.ID != want.ID || got.Name != want.Name || got.Status != want.Status {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestGetVM_NotFound(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms/", func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(t, w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"code": "not_found", "message": "vm not found"},
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	_, err := newClientAutoDir(t).VM(context.Background(), baseURL, "no-such-vm")
	if err == nil {
		t.Fatalf("expected 404 error")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
		t.Fatalf("err = %v; want AgentError 404", err)
	}
}

func TestListVMs_HappyPath(t *testing.T) {
	t.Parallel()

	want := []agentclient.AgentVM{
		{ID: uuid.New(), Name: "a", Status: "running", VCPUs: 2, MemoryMib: 1024},
		{ID: uuid.New(), Name: "b", Status: "stopped", VCPUs: 4, MemoryMib: 2048},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		jsonWrite(t, w, http.StatusOK, map[string]any{
			"data": want,
			"meta": map[string]any{"next_cursor": nil},
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	got, err := newClientAutoDir(t).ListVMs(context.Background(), baseURL)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != want[0].ID || got[1].ID != want[1].ID {
		t.Errorf("ids mismatch: got %v / %v, want %v / %v",
			got[0].ID, got[1].ID, want[0].ID, want[1].ID)
	}
}

func TestDeleteVM_HappyPath(t *testing.T) {
	t.Parallel()

	vmName := "demo"
	wantTaskID := uuid.New()
	wantIdem := "idem-" + uuid.NewString()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms/"+vmName, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if got := r.Header.Get("Idempotency-Key"); got != wantIdem {
			t.Errorf("Idempotency-Key = %q, want %q", got, wantIdem)
		}
		jsonWrite(t, w, http.StatusAccepted, map[string]any{
			"task_id": wantTaskID,
			"status":  "pending",
			"links":   map[string]any{"self": "/v1/tasks/" + wantTaskID.String()},
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	gotTaskID, err := newClientAutoDir(t).DeleteVM(context.Background(), baseURL, vmName, wantIdem)
	if err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if gotTaskID != wantTaskID {
		t.Errorf("task_id = %s, want %s", gotTaskID, wantTaskID)
	}
}

func TestDeleteVM_404IsAgentError(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms/", func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(t, w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"code": "not_found", "message": "vm not found"},
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	_, err := newClientAutoDir(t).DeleteVM(context.Background(), baseURL, "no-such-vm", "")
	if err == nil {
		t.Fatalf("expected 404 error")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
		t.Fatalf("err = %v; want AgentError 404", err)
	}
}

// TestDeleteVMOnSource_404IsSuccess asserts the post-cutover source cleanup
// wrapper treats an already-deleted source VM (HTTP 404 not_found) as success:
// the source agent now usually tears its departed VM down itself at the end of a
// successful outgoing-live migration, so the CP's slower async delete arrives to a
// VM that is already gone - which is exactly the success we want, not a "disk
// leaked" WARN. Other agent errors still propagate.
func TestDeleteVMOnSource_404IsSuccess(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms/", func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(t, w, http.StatusNotFound, map[string]any{
			"error": map[string]any{"code": "not_found", "message": "vm not found"},
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	if err := newClientAutoDir(t).DeleteVMOnSource(context.Background(), baseURL, "gone-vm"); err != nil {
		t.Errorf("DeleteVMOnSource on already-gone source = %v, want nil (idempotently already deleted)", err)
	}
}

// TestDeleteVMOnSource_500Propagates is the revert-confirm: a non-404 agent error
// (e.g. a 500) is NOT swallowed - it still surfaces so the worker logs the real
// leak. Only the already-deleted 404 is treated as success.
func TestDeleteVMOnSource_500Propagates(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/vms/", func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(t, w, http.StatusInternalServerError, map[string]any{
			"error": map[string]any{"code": "internal", "message": "boom"},
		})
	})
	_, baseURL := startAgentTLSServer(t, mux)

	err := newClientAutoDir(t).DeleteVMOnSource(context.Background(), baseURL, "some-vm")
	if err == nil {
		t.Fatalf("DeleteVMOnSource on 500 = nil, want error (non-404 must propagate)")
	}
	var ae *agentclient.AgentError
	if !errors.As(err, &ae) || ae.Status != http.StatusInternalServerError {
		t.Fatalf("err = %v; want AgentError 500", err)
	}
}

func TestPostVMCreate_ConnectionRefused(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	srv, baseURL := startAgentTLSServer(t, mux)
	srv.Close()

	_, err := newClientAutoDir(t).PostVMCreate(
		context.Background(), baseURL, "", validVMCreateRequest())
	if err == nil {
		t.Fatalf("expected connection error on closed listener")
	}
	if !strings.Contains(err.Error(), "agentclient: post vm create") {
		t.Errorf("err = %v; want post-vm-create wrapper", err)
	}
}

// Sanity: ensure unused import-suppressed; this drains io to keep
// imports honest if we ever need to add ReadAll-style assertions.
var _ = io.Discard

func TestVMCreateRequestNetworkConfigJSON(t *testing.T) {
	b, err := json.Marshal(agentclient.VMCreateRequest{UUID: uuid.New(), Name: "v", NetworkConfig: "network:\n  version: 2\n"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(b), `"network_config"`) {
		t.Errorf("Marshal omitted network_config: %s", b)
	}
	b2, err := json.Marshal(agentclient.VMCreateRequest{UUID: uuid.New(), Name: "v"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(b2), "network_config") {
		t.Errorf("Marshal included empty network_config: %s", b2)
	}
}
