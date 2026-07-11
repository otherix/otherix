// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// waitVMBody is the VM projection POST /v1/vms returns on create and that
// GET /v1/vms/{name} returns on each --wait poll. phase drives the
// status.phase the waiter branches on; reason/message are surfaced when
// the phase is a terminal failure. name echoes the request so the
// create-summary line and the GET path stay deterministic.
func waitVMBody(name, phase, reason, message string) []byte {
	return []byte(`{"id":"` + uuid.NewString() + `","name":"` + name + `","owner_id":"` + uuid.NewString() +
		`","pool":"default","architecture":"amd64","vcpus":2,"memory_mib":2048,` +
		`"status":{"phase":"` + phase + `","reason":"` + reason + `","message":"` + message + `"},` +
		`"desired_phase":"running","labels":{},` +
		`"created_at":"2026-05-10T10:00:00Z","updated_at":"2026-05-10T10:00:00Z"}`)
}

// waitVMServer wires a fake CP for the `vm create --wait` command path:
// POST /v1/vms returns 201 with the create body, GET /v1/vms/{name}
// returns the poll body (the phase the waiter observes). Mirrors the
// manifest-waiter test server in cmd/cli/create_wait_test.go but routed
// for the `vm create` command, which polls the VM projection by name.
func waitVMServer(t *testing.T, name string, createBody, pollBody []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(createBody)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/"+name:
			_, _ = w.Write(pollBody)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
}

func waitCreateArgs(name, timeout string) []string {
	return []string{
		"create", name,
		"--image-url", "https://example.com/ubuntu.qcow2",
		"--arch", "amd64",
		"--vcpus", "2",
		"--memory-mib", "2048",
		"--wait",
		"--wait-timeout", timeout,
	}
}

// TestVMCreateWaitRunning covers the success arm of waitForVMPhase: create
// returns 201 pending, the first GET reports phase=running, so the command
// exits 0 and prints the running summary line. This FAILS if the waiter
// ignored status.phase (it would never settle on running) — the assertion
// is not vacuous.
func TestVMCreateWaitRunning(t *testing.T) {
	t.Parallel()
	const name = "vm-wait-running"
	srv := waitVMServer(t, name,
		waitVMBody(name, "pending", "pending_schedule", ""),
		waitVMBody(name, "running", "", ""),
	)
	defer srv.Close()

	stdout, _, err := runVMCmd(t, srv.URL, waitCreateArgs(name, "10s"))
	if err != nil {
		t.Fatalf("create --wait error = %v", err)
	}
	if !strings.Contains(stdout, "vm running name="+name) {
		t.Errorf("stdout = %q, want the running summary line", stdout)
	}
}

// TestVMCreateWaitErrorPhase covers the failure arm: the VM reaches the
// terminal error phase, so the command returns a non-nil error and the
// reason/message is surfaced. FAILS if errors were swallowed or the reason
// dropped — not vacuous.
func TestVMCreateWaitErrorPhase(t *testing.T) {
	t.Parallel()
	const name = "vm-wait-error"
	srv := waitVMServer(t, name,
		waitVMBody(name, "pending", "pending_schedule", ""),
		waitVMBody(name, "error", "image_unavailable", "boom"),
	)
	defer srv.Close()

	stdout, stderr, err := runVMCmd(t, srv.URL, waitCreateArgs(name, "10s"))
	if err == nil {
		t.Fatalf("expected error when the VM enters the error phase")
	}
	if strings.Contains(stdout, "vm running name=") {
		t.Errorf("an error-phase VM must not print the running line; stdout=%q", stdout)
	}
	// The per-poll reason goes to stderr; the terminal error string carries
	// the reason+message too. Accept either surface so the test pins the
	// "reason reaches the operator" contract without over-fitting the sink.
	if !strings.Contains(stderr, "image_unavailable") && !strings.Contains(err.Error(), "image_unavailable") {
		t.Errorf("want the error reason surfaced; stderr=%q err=%v", stderr, err)
	}
}

// TestVMCreateWaitFailedPhase covers the second terminal failure phase
// (failed, distinct from error): it must not be mistaken for success.
// FAILS if the waiter treated any non-running terminal phase as ready.
func TestVMCreateWaitFailedPhase(t *testing.T) {
	t.Parallel()
	const name = "vm-wait-failed"
	srv := waitVMServer(t, name,
		waitVMBody(name, "pending", "pending_schedule", ""),
		waitVMBody(name, "failed", "create_failed", "agent rejected"),
	)
	defer srv.Close()

	stdout, stderr, err := runVMCmd(t, srv.URL, waitCreateArgs(name, "10s"))
	if err == nil {
		t.Fatalf("expected error when the VM enters the failed phase")
	}
	if strings.Contains(stdout, "vm running name=") {
		t.Errorf("a failed VM must not print the running line; stdout=%q", stdout)
	}
	if !strings.Contains(stderr, "create_failed") && !strings.Contains(err.Error(), "create_failed") {
		t.Errorf("want the failed reason surfaced; stderr=%q err=%v", stderr, err)
	}
}

// TestVMCreateWaitStuckPendingTimesOut is the safety-critical arm: a VM
// stuck at the non-terminal pending phase (the scheduler never binds it)
// must time out, never be read as success. A false-success would mislead an
// operator into a destructive retry. Kept fast with a short --wait-timeout:
// waitForVMPhase polls once immediately, then sleeps with the per-iteration
// timer bounded by the wait deadline (sleepVMCtx returns ctx.Err() the
// moment the 150ms budget elapses), so the case resolves in well under a
// second despite the 1s backoff floor.
func TestVMCreateWaitStuckPendingTimesOut(t *testing.T) {
	t.Parallel()
	const name = "vm-wait-pending"
	pending := waitVMBody(name, "pending", "pending_schedule", "")
	srv := waitVMServer(t, name, pending, pending)
	defer srv.Close()

	stdout, _, err := runVMCmd(t, srv.URL, waitCreateArgs(name, "150ms"))
	if err == nil {
		t.Fatalf("expected a timeout for a VM stuck at the pending phase")
	}
	if strings.Contains(stdout, "vm running name=") {
		t.Errorf("a stuck-pending VM must never be reported running; stdout=%q", stdout)
	}
}
