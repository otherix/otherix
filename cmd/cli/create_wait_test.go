// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fastPoolPoll temporarily lowers the pool reconciliation poll interval
// so loop-exercising tests run quickly, restoring it on cleanup.
func fastPoolPoll(t *testing.T) {
	t.Helper()
	prev := poolReconcilePoll
	poolReconcilePoll = 5 * time.Millisecond
	t.Cleanup(func() { poolReconcilePoll = prev })
}

const waitManifest = `apiVersion: otherix/v1
kind: VM
metadata: { name: web-1 }
spec: { imageURL: https://x/u.qcow2, arch: arm64 }
`

func TestCreateWaitPollsTaskToSuccess(t *testing.T) {
	taskID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/vms":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"` + taskID + `","status":"pending","links":{"self":"/v1/tasks/` + taskID + `"}}`))
		case "/v1/tasks/" + taskID:
			_, _ = w.Write([]byte(`{"id":"` + taskID + `","type":"vm.create","status":"success","progress":null,"resource_type":"vm","resource_id":null,"result":null,"error":null,"attempts":1,"max_attempts":1,"created_at":"2026-06-05T00:00:00Z","started_at":null,"finished_at":null}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, _, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, waitManifest), "--wait", "--wait-timeout", "10s")
	if err != nil {
		t.Fatalf("create --wait error = %v", err)
	}
	if !strings.Contains(stdout, "ready") {
		t.Errorf("stdout = %q, want a readiness line", stdout)
	}
}

const waitPoolManifest = `apiVersion: otherix/v1
kind: StoragePool
metadata: { name: pool-1 }
spec: { path: /opt/p, node: node-1 }
`

func TestCreateWaitPoolReconciliation(t *testing.T) {
	poolID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/storage-pools":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"` + poolID + `","node":"node-1","name":"pool-1","type":"local_dir","path":"/opt/p","reconciliation_status":"pending","config":{},"images":[],"created_at":"2026-06-05T00:00:00Z","updated_at":"2026-06-05T00:00:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/storage-pools/"+poolID:
			_, _ = w.Write([]byte(`{"id":"` + poolID + `","node":"node-1","name":"pool-1","type":"local_dir","path":"/opt/p","reconciliation_status":"ready","config":{},"images":[],"created_at":"2026-06-05T00:00:00Z","updated_at":"2026-06-05T00:00:00Z"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, _, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, waitPoolManifest), "--wait", "--wait-timeout", "10s")
	if err != nil {
		t.Fatalf("create --wait error = %v", err)
	}
	if !strings.Contains(stdout, "ready") {
		t.Errorf("stdout = %q, want a readiness line", stdout)
	}
}

// TestCreateWaitPoolReconcilePending exercises the pool poll loop body:
// the GET returns pending first, then ready, so the waiter must loop at
// least twice before reporting ready.
func TestCreateWaitPoolReconcilePending(t *testing.T) {
	fastPoolPoll(t)
	poolID := uuid.NewString()
	var gets int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/v1/storage-pools" {
			_, _ = w.Write([]byte(`{"id":"` + poolID + `","name":"p1","node":"node-1","type":"local_dir","path":"/opt/p","reconciliation_status":"pending"}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/storage-pools/"+poolID {
			gets++
			status := "pending"
			if gets >= 2 {
				status = "ready"
			}
			_, _ = w.Write([]byte(`{"id":"` + poolID + `","name":"p1","node":"node-1","type":"local_dir","path":"/opt/p","reconciliation_status":"` + status + `"}`))
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	const m = "apiVersion: otherix/v1\nkind: StoragePool\nmetadata: { name: p1 }\nspec: { path: /opt/p, node: node-1 }\n"
	stdout, _, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, m), "--wait", "--wait-timeout", "10s")
	if err != nil {
		t.Fatalf("create --wait error = %v", err)
	}
	if gets < 2 {
		t.Errorf("expected the pool poll loop to GET at least twice (pending then ready), got %d", gets)
	}
	if !strings.Contains(stdout, "ready") {
		t.Errorf("stdout = %q, want a ready line", stdout)
	}
}

// TestCreateWaitPoolReconcileFailed pins the no-false-success property for
// the pool waiter's terminal-failure arm: a pool whose reconciliation
// reaches "failed" must surface a non-zero exit and never a readiness
// line. Without the case "failed" arm the waiter would poll a terminally
// failed pool until the timeout instead of reporting promptly.
func TestCreateWaitPoolReconcileFailed(t *testing.T) {
	fastPoolPoll(t)
	poolID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/v1/storage-pools" {
			_, _ = w.Write([]byte(`{"id":"` + poolID + `","name":"p1","node":"node-1","type":"local_dir","path":"/opt/p","reconciliation_status":"pending"}`))
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/storage-pools/"+poolID {
			_, _ = w.Write([]byte(`{"id":"` + poolID + `","name":"p1","node":"node-1","type":"local_dir","path":"/opt/p","reconciliation_status":"failed"}`))
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	const m = "apiVersion: otherix/v1\nkind: StoragePool\nmetadata: { name: p1 }\nspec: { path: /opt/p, node: node-1 }\n"
	stdout, stderr, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, m), "--wait", "--wait-timeout", "10s")
	if err == nil {
		t.Fatalf("expected error when pool reconciliation fails")
	}
	if strings.Contains(stdout, "ready") {
		t.Errorf("a failed pool must not print a readiness line; stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "reconciliation failed") {
		t.Errorf("stderr = %q, want the reconciliation-failed message", stderr)
	}
}

// TestCreateWaitPoolTimeoutCapped proves Fix #5: a never-ready pool with
// a poll interval far larger than the wait timeout must still return
// quickly, because the per-iteration sleep is capped at the remaining
// deadline. Un-capped, the first sleep would block ~10s.
func TestCreateWaitPoolTimeoutCapped(t *testing.T) {
	prev := poolReconcilePoll
	poolReconcilePoll = 10 * time.Second // large on purpose
	t.Cleanup(func() { poolReconcilePoll = prev })
	poolID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + poolID + `","name":"p1","node":"node-1","type":"local_dir","path":"/opt/p","reconciliation_status":"pending"}`))
	}))
	defer srv.Close()
	const m = "apiVersion: otherix/v1\nkind: StoragePool\nmetadata: { name: p1 }\nspec: { path: /opt/p, node: node-1 }\n"
	start := time.Now()
	_, stderr, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, m), "--wait", "--wait-timeout", "50ms")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error for a never-ready pool")
	}
	if elapsed > 3*time.Second {
		t.Errorf("wait took %v; sleep was not capped to the remaining deadline (poll interval was 10s)", elapsed)
	}
	if !strings.Contains(stderr, "not ready") && !strings.Contains(stderr, "did not reach ready") {
		t.Errorf("stderr = %q, want a reconciliation-timeout message", stderr)
	}
}

// TestCreateWaitPoolGetHangBoundedByTimeout proves Fix R1: a hung pool
// GET must be bounded by --wait-timeout, not the cpclient http.Client's
// fixed 30s timeout. The handler blocks the GET until the test ends, so
// without the per-request deadline the wait would run ~30s.
func TestCreateWaitPoolGetHangBoundedByTimeout(t *testing.T) {
	poolID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id":"` + poolID + `","name":"p1","node":"node-1","type":"local_dir","path":"/opt/p","reconciliation_status":"pending"}`))
			return
		}
		// Hang the GET until the CLIENT cancels it (the per-request
		// reqCtx deadline bounded by --wait-timeout). Using r.Context()
		// instead of an external channel lets srv.Close() complete
		// cleanly once the client cancels - no deadlock on the handler.
		<-r.Context().Done()
	}))
	defer srv.Close()
	const m = "apiVersion: otherix/v1\nkind: StoragePool\nmetadata: { name: p1 }\nspec: { path: /opt/p, node: node-1 }\n"
	start := time.Now()
	_, _, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, m), "--wait", "--wait-timeout", "1s")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error for a hung pool GET")
	}
	if elapsed > 10*time.Second {
		t.Errorf("wait took %v; a hung pool GET was not bounded by --wait-timeout (would be ~30s http client timeout)", elapsed)
	}
}

// TestCreateWaitPoolRetriesTransient proves Fix R2: a single transient
// 503 during the reconcile poll must be retried (not surfaced as a false
// "not ready"), then the wait converges once the pool reports ready.
func TestCreateWaitPoolRetriesTransient(t *testing.T) {
	fastPoolPoll(t)
	poolID := uuid.NewString()
	var gets int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"id":"` + poolID + `","name":"p1","node":"node-1","type":"local_dir","path":"/opt/p","reconciliation_status":"pending"}`))
			return
		}
		gets++
		if gets == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // transient blip on first poll
			_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"blip"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + poolID + `","name":"p1","node":"node-1","type":"local_dir","path":"/opt/p","reconciliation_status":"ready"}`))
	}))
	defer srv.Close()
	const m = "apiVersion: otherix/v1\nkind: StoragePool\nmetadata: { name: p1 }\nspec: { path: /opt/p, node: node-1 }\n"
	stdout, _, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, m), "--wait", "--wait-timeout", "10s")
	if err != nil {
		t.Fatalf("transient blip should be retried, got error: %v", err)
	}
	if gets < 2 {
		t.Errorf("expected a retry after the 503 (>=2 GETs), got %d", gets)
	}
	if !strings.Contains(stdout, "ready") {
		t.Errorf("stdout = %q, want ready after retry", stdout)
	}
}

// TestCreateWaitVMTaskFailure confirms a create task reaching terminal
// failed surfaces the task error envelope and a non-zero exit.
func TestCreateWaitVMTaskFailure(t *testing.T) {
	taskID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/vms":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"` + taskID + `","status":"pending","links":{"self":"/v1/tasks/` + taskID + `"}}`))
		case "/v1/tasks/" + taskID:
			_, _ = w.Write([]byte(`{"id":"` + taskID + `","type":"vm.create","status":"failed","progress":null,"resource_type":"vm","resource_id":null,"result":null,"error":{"code":"image_unavailable","message":"boom"},"attempts":1,"max_attempts":1,"created_at":"2026-06-05T00:00:00Z","started_at":null,"finished_at":null}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	stdout, stderr, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, waitManifest), "--wait", "--wait-timeout", "10s")
	if err == nil {
		t.Fatalf("expected error when the create task fails")
	}
	if !strings.Contains(stderr, "image_unavailable") && !strings.Contains(stdout, "image_unavailable") {
		t.Errorf("want the task error surfaced; stdout=%q stderr=%q", stdout, stderr)
	}
}

// TestCreateWaitVMTaskCancelled confirms a terminal cancelled task is not
// mistaken for success: no readiness line, non-zero exit. The waiter must
// never print "ready" for any status other than the literal "success".
func TestCreateWaitVMTaskCancelled(t *testing.T) {
	taskID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/vms":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"` + taskID + `","status":"pending","links":{"self":"/v1/tasks/` + taskID + `"}}`))
		case "/v1/tasks/" + taskID:
			_, _ = w.Write([]byte(`{"id":"` + taskID + `","type":"vm.create","status":"cancelled","progress":null,"resource_type":"vm","resource_id":null,"result":null,"error":null,"attempts":1,"max_attempts":1,"created_at":"2026-06-05T00:00:00Z","started_at":null,"finished_at":null}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	stdout, stderr, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, waitManifest), "--wait", "--wait-timeout", "10s")
	if err == nil {
		t.Fatalf("expected error when the create task is cancelled")
	}
	if strings.Contains(stdout, "ready") {
		t.Errorf("a cancelled task must not be reported ready; stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "cancelled") {
		t.Errorf("want the cancelled status surfaced; stderr=%q", stderr)
	}
}

// TestCreateWaitVMUnknownStatusNeverReady proves the fail-safe core: a task
// stuck at an empty/unknown status (never terminal) must time out, never be
// read as success. A false-success here would mislead an operator into a
// destructive retry, so this is the single most safety-critical property.
func TestCreateWaitVMUnknownStatusNeverReady(t *testing.T) {
	taskID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/vms":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"` + taskID + `","status":"pending","links":{"self":"/v1/tasks/` + taskID + `"}}`))
		case "/v1/tasks/" + taskID:
			// Empty status is not terminal: the waiter must keep polling
			// until the deadline, then time out - never report ready.
			_, _ = w.Write([]byte(`{"id":"` + taskID + `","type":"vm.create","status":"","progress":null,"resource_type":"vm","resource_id":null,"result":null,"error":null,"attempts":1,"max_attempts":1,"created_at":"2026-06-05T00:00:00Z","started_at":null,"finished_at":null}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	stdout, _, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, waitManifest), "--wait", "--wait-timeout", "150ms")
	if err == nil {
		t.Fatalf("expected a timeout for a task stuck at unknown status")
	}
	if strings.Contains(stdout, "ready") {
		t.Errorf("an unknown-status task must never be reported ready; stdout=%q", stdout)
	}
}
