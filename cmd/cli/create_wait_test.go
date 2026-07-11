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

// vmCreatedBody is the 201 admission-only response: the VM view in the
// given phase (no task envelope). The manifest --wait path polls the VM
// projection by name, not a task.
func vmCreatedBody(phase string) string {
	return `{"id":"` + uuid.NewString() + `","name":"web-1","owner_id":"` + uuid.NewString() +
		`","pool":"default","architecture":"arm64","vcpus":2,"memory_mib":2048,` +
		`"status":{"phase":"` + phase + `"},"desired_phase":"running","labels":{},` +
		`"created_at":"2026-06-05T00:00:00Z","updated_at":"2026-06-05T00:00:00Z"}`
}

func TestCreateWaitPollsVMToRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(vmCreatedBody("pending")))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/web-1":
			_, _ = w.Write([]byte(vmCreatedBody("running")))
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
	// committed=true path: the pool WAS created, only reconciliation did not
	// converge, so the summary must use the distinct "but not ready" wording
	// rather than the "failed:" wording reserved for a create that never
	// happened server-side.
	if !strings.Contains(stderr, "but not ready") {
		t.Errorf("stderr = %q, want the committed-but-not-ready wording", stderr)
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

// vmErrorBody is the VM projection in a terminal error phase carrying a
// scheduling/runtime reason+message, which the manifest --wait path
// surfaces verbatim.
func vmErrorBody(phase, reason, message string) string {
	return `{"id":"` + uuid.NewString() + `","name":"web-1","owner_id":"` + uuid.NewString() +
		`","pool":"default","architecture":"arm64","vcpus":2,"memory_mib":2048,` +
		`"status":{"phase":"` + phase + `","reason":"` + reason + `","message":"` + message + `"},` +
		`"desired_phase":"running","labels":{},"created_at":"2026-06-05T00:00:00Z","updated_at":"2026-06-05T00:00:00Z"}`
}

// TestCreateWaitVMPhaseError confirms a VM reaching the terminal error
// phase surfaces its scheduling/runtime reason and a non-zero exit.
func TestCreateWaitVMPhaseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(vmCreatedBody("pending")))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/web-1":
			_, _ = w.Write([]byte(vmErrorBody("error", "image_unavailable", "boom")))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	stdout, stderr, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, waitManifest), "--wait", "--wait-timeout", "10s")
	if err == nil {
		t.Fatalf("expected error when the VM enters the error phase")
	}
	if !strings.Contains(stderr, "image_unavailable") && !strings.Contains(stdout, "image_unavailable") {
		t.Errorf("want the error reason surfaced; stdout=%q stderr=%q", stdout, stderr)
	}
}

// TestCreateWaitVMPhaseFailed confirms the second terminal failure phase
// (failed, distinct from error) is not mistaken for success: no readiness
// line, non-zero exit, and the reason surfaced. The waiter must only
// print "ready" for the running phase.
func TestCreateWaitVMPhaseFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(vmCreatedBody("pending")))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/web-1":
			_, _ = w.Write([]byte(vmErrorBody("failed", "create_failed", "agent rejected")))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	stdout, stderr, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, waitManifest), "--wait", "--wait-timeout", "10s")
	if err == nil {
		t.Fatalf("expected error when the VM enters the failed phase")
	}
	if strings.Contains(stdout, "ready") {
		t.Errorf("a failed VM must not be reported ready; stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "create_failed") {
		t.Errorf("want the failed reason surfaced; stderr=%q", stderr)
	}
}

// TestCreateWaitVMStuckPendingNeverReady proves the fail-safe core: a VM
// stuck at a non-terminal phase (pending - the scheduler never binds it)
// must time out, never be read as success. A false-success here would
// mislead an operator into a destructive retry, so this is the single
// most safety-critical property.
func TestCreateWaitVMStuckPendingNeverReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(vmCreatedBody("pending")))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/web-1":
			// Pending is not terminal: the waiter must keep polling until
			// the deadline, then time out - never report ready.
			_, _ = w.Write([]byte(vmCreatedBody("pending")))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	stdout, _, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, waitManifest), "--wait", "--wait-timeout", "150ms")
	if err == nil {
		t.Fatalf("expected a timeout for a VM stuck at the pending phase")
	}
	if strings.Contains(stdout, "ready") {
		t.Errorf("a stuck-pending VM must never be reported ready; stdout=%q", stdout)
	}
}
