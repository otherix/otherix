// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cpclient_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// fixtureClient builds a Client pointing at srv.URL with a stable test
// token. The server's handler observes the token via Authorization
// header, so tests assert auth is wired without parsing it themselves.
func fixtureClient(t *testing.T, srv *httptest.Server) *cpclient.Client {
	t.Helper()
	c, err := cpclient.New(cpclient.Options{
		BaseURL:    srv.URL,
		Token:      "test-token",
		Timeout:    2 * time.Second,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("cpclient.New: %v", err)
	}
	return c
}

func TestNew_RejectsEmptyBaseURL(t *testing.T) {
	t.Parallel()
	if _, err := cpclient.New(cpclient.Options{Token: "x"}); err == nil {
		t.Fatalf("expected error for empty BaseURL")
	}
}

func TestNew_RejectsEmptyToken(t *testing.T) {
	t.Parallel()
	if _, err := cpclient.New(cpclient.Options{BaseURL: "http://x"}); err == nil {
		t.Fatalf("expected error for empty Token")
	}
}

func TestCreateVM_HappyPath(t *testing.T) {
	t.Parallel()
	vmID := uuid.NewString()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/vms" {
			t.Errorf("path = %s, want /v1/vms", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"name":"demo"`) {
			t.Errorf("body missing name field: %s", body)
		}
		// Admission-only: the server persists the VM as pending and
		// returns 201 + the VM view, not a 202 task envelope.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":            vmID,
			"name":          "demo",
			"owner_id":      uuid.NewString(),
			"image_url":     "https://example.com/img.qcow2",
			"pool":          "default",
			"architecture":  "amd64",
			"vcpus":         2,
			"memory_mb":     2048,
			"status":        map[string]any{"phase": "pending", "reason": "pending_schedule"},
			"desired_phase": "running",
			"labels":        map[string]any{},
			"created_at":    "2026-05-10T10:00:00Z",
			"updated_at":    "2026-05-10T10:00:00Z",
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, raw, err := c.CreateVM(context.Background(), cpclient.CreateVMRequest{
		Name:     "demo",
		ImageURL: "https://example.com/img.qcow2",
		Arch:     "amd64",
		Pool:     uuid.NewString(),
		VCPUs:    2,
		MemoryMB: 2048,
	})
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if got.ID != vmID {
		t.Errorf("ID = %s, want %s", got.ID, vmID)
	}
	if got.Name != "demo" {
		t.Errorf("Name = %s, want demo", got.Name)
	}
	if got.Status.Phase != "pending" {
		t.Errorf("Status.Phase = %s, want pending", got.Status.Phase)
	}
	if got.Status.Reason != "pending_schedule" {
		t.Errorf("Status.Reason = %s, want pending_schedule", got.Status.Reason)
	}
	if !strings.Contains(string(raw), `"phase":"pending"`) {
		t.Errorf("raw body = %s, want nested status phase", raw)
	}
}

func TestCreateVM_APIError404(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"vm not found"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, _, err := c.CreateVM(context.Background(), cpclient.CreateVMRequest{
		Name: "demo", ImageURL: "https://example.com/img.qcow2", Arch: "amd64", Pool: uuid.NewString(), VCPUs: 2, MemoryMB: 1024,
	})
	if err == nil {
		t.Fatalf("expected error for 404")
	}
	var ae *cpclient.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v; want *APIError", err)
	}
	if ae.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d, want 404", ae.StatusCode)
	}
	if ae.Code != "not_found" {
		t.Errorf("Code = %q, want not_found", ae.Code)
	}
	if ae.IsRetryable() {
		t.Errorf("404 must not be retryable")
	}
}

func TestCreateVM_APIError5xxRetryable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"overloaded"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, _, err := c.CreateVM(context.Background(), cpclient.CreateVMRequest{
		Name: "x", ImageURL: "https://example.com/img.qcow2", Arch: "amd64", Pool: uuid.NewString(), VCPUs: 1, MemoryMB: 1024,
	})
	var ae *cpclient.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v; want *APIError", err)
	}
	if !ae.IsRetryable() {
		t.Errorf("503 must be retryable")
	}
}

func TestGetVM_HappyPath(t *testing.T) {
	t.Parallel()
	vmID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":            vmID.String(),
			"name":          "demo",
			"owner_id":      uuid.NewString(),
			"image_url":     "https://example.test/img.qcow2",
			"pool_id":       uuid.NewString(),
			"node_id":       uuid.NewString(),
			"architecture":  "amd64",
			"vcpus":         2,
			"memory_mb":     2048,
			"status":        map[string]any{"phase": "running"},
			"desired_phase": "running",
			"labels":        map[string]any{},
			"created_at":    "2026-05-10T10:00:00Z",
			"updated_at":    "2026-05-10T10:01:00Z",
		})
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, _, err := c.VM(context.Background(), vmID.String())
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if got.ID != vmID.String() || got.Name != "demo" || got.Status.Phase != "running" {
		t.Errorf("got = %+v, want id=%s name=demo status.phase=running", got, vmID)
	}
}

func TestGetVM_AcceptsName(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server resolves names; cpclient forwards the raw identifier
		// verbatim via url.PathEscape (which leaves bare letters /
		// digits / hyphens unchanged).
		if r.URL.Path != "/v1/vms/demo-vm" {
			t.Errorf("path = %s, want /v1/vms/demo-vm", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"00000000-0000-0000-0000-000000000001","name":"demo-vm","owner_id":"00000000-0000-0000-0000-000000000002","pool":"default","architecture":"amd64","vcpus":2,"memory_mb":2048,"status":{"phase":"running"},"desired_phase":"running","labels":{},"created_at":"2026-05-10T10:00:00Z","updated_at":"2026-05-10T10:00:00Z"}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, _, err := c.VM(context.Background(), "demo-vm")
	if err != nil {
		t.Fatalf("GetVM: %v", err)
	}
	if got.Name != "demo-vm" {
		t.Errorf("Name = %s, want demo-vm", got.Name)
	}
}

func TestListVMs_QueryStringEncoded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("limit") != "10" || q.Get("status") != "running" {
			t.Errorf("query = %v, want limit=10 status=running", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if _, err := c.ListVMs(context.Background(), cpclient.ListVMsParams{
		Limit:  10,
		Status: "running",
	}); err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
}

func TestDeleteVM_HappyPath(t *testing.T) {
	t.Parallel()
	taskID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"task_id":"` + taskID + `","status":"pending","links":{"self":"/v1/tasks/` + taskID + `"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.DeleteVM(context.Background(), uuid.New().String())
	if err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
	if got.TaskID != taskID {
		t.Errorf("TaskID = %s, want %s", got.TaskID, taskID)
	}
}

func TestDeleteVM_PendingNoContent(t *testing.T) {
	t.Parallel()
	// A pending (unscheduled) VM is deleted CP-side and returns 204 No Content
	// with an empty body - DeleteVM must treat that as success (empty
	// TaskAccepted), not try to decode an absent JSON task envelope.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.DeleteVM(context.Background(), "pending-vm")
	if err != nil {
		t.Fatalf("DeleteVM(204) = %v, want nil", err)
	}
	if got.TaskID != "" {
		t.Errorf("TaskID = %q, want empty (synchronous delete)", got.TaskID)
	}
}

func TestDeleteVM_AcceptsName(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/vms/demo-vm" {
			t.Errorf("path = %s, want /v1/vms/demo-vm", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"task_id":"00000000-0000-0000-0000-000000000099","status":"pending","links":{"self":"/v1/tasks/00000000-0000-0000-0000-000000000099"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	if _, err := c.DeleteVM(context.Background(), "demo-vm"); err != nil {
		t.Fatalf("DeleteVM: %v", err)
	}
}

func TestWaitTask_TerminalSuccess(t *testing.T) {
	t.Parallel()
	taskID := uuid.New()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// First two polls return running; third returns success.
		status := "running"
		if n >= 3 {
			status = "success"
		}
		_, _ = w.Write([]byte(`{"id":"` + taskID.String() + `","type":"vm.create","status":"` + status + `","resource_type":"vm","attempts":1,"max_attempts":25,"created_at":"2026-05-10T10:00:00Z"}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.WaitTask(context.Background(), taskID, cpclient.WaitOptions{
		Initial: 10 * time.Millisecond,
		Max:     20 * time.Millisecond,
		Timeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("WaitTask: %v", err)
	}
	if got.Status != "success" {
		t.Errorf("Status = %s, want success", got.Status)
	}
	if calls.Load() < 3 {
		t.Errorf("polls = %d, want ≥ 3", calls.Load())
	}
}

func TestWaitTask_TerminalFailedSurfacesEnvelope(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"00000000-0000-0000-0000-000000000000","type":"vm.create","status":"failed","resource_type":"vm","attempts":1,"max_attempts":25,"created_at":"2026-05-10T10:00:00Z","error":{"code":"qemu_spawn_failed","message":"kvm not available"}}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.WaitTask(context.Background(), uuid.New(), cpclient.WaitOptions{
		Initial: 5 * time.Millisecond,
		Timeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("WaitTask: %v (terminal-failed must NOT surface as Go error)", err)
	}
	if got.Status != "failed" {
		t.Errorf("Status = %s, want failed", got.Status)
	}
	env, err := got.DecodeError()
	if err != nil || env == nil {
		t.Fatalf("DecodeError = (%+v, %v); want non-nil envelope", env, err)
	}
	if env.Code != "qemu_spawn_failed" {
		t.Errorf("env.Code = %s, want qemu_spawn_failed", env.Code)
	}
}

func TestWaitTask_TimeoutSurfacesDeadlineExceeded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"00000000-0000-0000-0000-000000000000","type":"vm.create","status":"running","resource_type":"vm","attempts":1,"max_attempts":25,"created_at":"2026-05-10T10:00:00Z"}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	_, err := c.WaitTask(context.Background(), uuid.New(), cpclient.WaitOptions{
		Initial: 30 * time.Millisecond,
		Max:     30 * time.Millisecond,
		Timeout: 100 * time.Millisecond,
	})
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v; want context.DeadlineExceeded chain", err)
	}
}

func TestWaitTask_RetriesTransient5xx(t *testing.T) {
	t.Parallel()
	taskID := uuid.New()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// First call: 503; second: success.
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"overloaded"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + taskID.String() + `","type":"vm.create","status":"success","resource_type":"vm","attempts":1,"max_attempts":25,"created_at":"2026-05-10T10:00:00Z"}`))
	}))
	defer srv.Close()

	c := fixtureClient(t, srv)
	got, err := c.WaitTask(context.Background(), taskID, cpclient.WaitOptions{
		Initial: 5 * time.Millisecond,
		Timeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("WaitTask: %v (5xx must retry transparently)", err)
	}
	if got.Status != "success" {
		t.Errorf("Status = %s, want success", got.Status)
	}
}

func TestClientTrustsCustomCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	// PEM-encode the server's own cert as the CA the client must trust.
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	// Without the CA: handshake fails.
	bad, err := cpclient.New(cpclient.Options{BaseURL: srv.URL, Token: "otx_x"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, gerr := bad.HTTPClient().Get(srv.URL); gerr == nil {
		t.Error("request without CA succeeded, want TLS verification error")
	}

	// With the CA: handshake succeeds.
	good, err := cpclient.New(cpclient.Options{BaseURL: srv.URL, Token: "otx_x", CACertPEM: caPEM})
	if err != nil {
		t.Fatalf("New with CA: %v", err)
	}
	resp, gerr := good.HTTPClient().Get(srv.URL)
	if gerr != nil {
		t.Fatalf("request with CA failed: %v", gerr)
	}
	_ = resp.Body.Close()
}

func TestClientInsecureSkipVerify(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c, err := cpclient.New(cpclient.Options{BaseURL: srv.URL, Token: "otx_x", InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, gerr := c.HTTPClient().Get(srv.URL)
	if gerr != nil {
		t.Fatalf("insecure request failed: %v", gerr)
	}
	_ = resp.Body.Close()
}

func TestNetworkFailureSurfacesAsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	srv.Close() // immediately closed → connection refused

	c, err := cpclient.New(cpclient.Options{
		BaseURL: srv.URL,
		Token:   "test-token",
		Timeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = c.VM(context.Background(), uuid.New().String())
	if err == nil {
		t.Fatalf("expected network error")
	}
	if !strings.Contains(err.Error(), "cpclient: request") {
		t.Errorf("err = %v; want 'cpclient: request' wrapper", err)
	}
}
