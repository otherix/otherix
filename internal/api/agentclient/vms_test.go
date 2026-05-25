// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
)

// newClientAutoDir creates а fresh tempdir, drops cert material in
// it via writeMaterial, и constructs an agentclient. Used по тестов
// что don't need access к the underlying dir.
func newClientAutoDir(t *testing.T) *agentclient.Client {
	t.Helper()
	dir := writeMaterial(t)
	return newClient(t, dir)
}

func validVMCreateRequest() agentclient.VMCreateRequest {
	return agentclient.VMCreateRequest{
		UUID:             uuid.New(),
		Name:             "test-vm",
		VCPUs:            2,
		MemoryMB:         2048,
		Pool:             "test-pool",
		TemplateChecksum: strings.Repeat("a", 64),
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
		t.Errorf("IsRetryable() = false, want true для 5xx")
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
		MemoryMB:     4096,
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
			"memory_mb":    want.MemoryMB,
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
		{ID: uuid.New(), Name: "a", Status: "running", VCPUs: 2, MemoryMB: 1024},
		{ID: uuid.New(), Name: "b", Status: "stopped", VCPUs: 4, MemoryMB: 2048},
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
