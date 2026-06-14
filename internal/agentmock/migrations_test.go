// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/agentmock"
)

// startIncoming POSTs /v1/vms/{vm}/migrations/incoming with the
// CP-allocated migration id and returns the parsed
// MigrationIncomingResponse. Fails fast on non-200 / decode error.
func startIncoming(t *testing.T, m mockURL, vmName string, migrationID uuid.UUID) agentapi.MigrationIncomingResponse {
	t.Helper()
	reqBody := agentapi.MigrationIncomingRequest{
		MigrationID:        migrationID,
		Mode:               agentapi.MigrationIncomingRequestModeLive,
		SourceNodeIdentity: strPtr("CN=node-source"),
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal incoming body: %v", err)
	}
	url := m.URL() + "/v1/vms/" + vmName + "/migrations/incoming"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("build incoming request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("start incoming: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("start incoming status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	var out agentapi.MigrationIncomingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode incoming response: %v", err)
	}
	return out
}

// startOutgoing POSTs /v1/vms/{vm}/migrations/outgoing and returns the
// minted async task id. Fails fast on non-202 / decode error. nbd is the
// target's NBD disk-export endpoint (from the incoming response); the CP
// relays it onto the outgoing request for live migrations, so the live
// nbd-required guard is satisfied. Pass "" only for offline flows.
func startOutgoing(t *testing.T, m mockURL, vmName string, migrationID uuid.UUID, target, token, nbd string) uuid.UUID {
	t.Helper()
	reqBody := agentapi.MigrationOutgoingRequest{
		AuthToken:          token,
		MigrationID:        migrationID,
		Mode:               agentapi.MigrationOutgoingRequestMode("live"),
		TargetEndpoint:     target,
		TargetNodeIdentity: strPtr("node-target.agents.otherix.local"),
	}
	if nbd != "" {
		reqBody.NbdEndpoint = &nbd
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal outgoing body: %v", err)
	}
	url := m.URL() + "/v1/vms/" + vmName + "/migrations/outgoing"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("build outgoing request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("start outgoing: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("start outgoing status = %d, want 202; body=%s", resp.StatusCode, body)
	}
	var out agentapi.AsyncTaskAccepted
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode outgoing accepted: %v", err)
	}
	if out.TaskID == uuid.Nil {
		t.Fatalf("outgoing returned zero task id")
	}
	return out.TaskID
}

// getMigration GETs /v1/vms/{vm}/migrations/{id} and returns
// (status, Migration).
func getMigration(t *testing.T, m mockURL, vmName string, migrationID uuid.UUID) (int, agentapi.Migration) {
	t.Helper()
	url := m.URL() + "/v1/vms/" + vmName + "/migrations/" + migrationID.String()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build migration get request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get migration: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, agentapi.Migration{}
	}
	var out agentapi.Migration
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode migration: %v", err)
	}
	return resp.StatusCode, out
}

// cancelMigration POSTs /v1/vms/{vm}/migrations/{id}/cancel and returns
// the resulting Migration.
func cancelMigration(t *testing.T, m mockURL, vmName string, migrationID uuid.UUID) agentapi.Migration {
	t.Helper()
	url := m.URL() + "/v1/vms/" + vmName + "/migrations/" + migrationID.String() + "/cancel"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, http.NoBody)
	if err != nil {
		t.Fatalf("build cancel request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cancel migration: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("cancel status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	var out agentapi.Migration
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode cancel migration: %v", err)
	}
	return out
}

// TestMigration_TwoPhaseHappyPath drives the full mock contract:
// StartIncoming on the target mints a listen endpoint + auth token;
// StartOutgoing on the source returns a task id that flips
// pending->running->success after the staged delay; GetMigration
// advances setup->active->completed in lockstep.
func TestMigration_TwoPhaseHappyPath(t *testing.T) {
	target := startMock(t)
	source := startMock(t)

	migrationID := uuid.New()
	vmName := "demo"

	// Phase 1: target prepares to receive.
	in := startIncoming(t, target, vmName, migrationID)
	if in.MigrationID != migrationID {
		t.Errorf("incoming migration_id = %v, want %v", in.MigrationID, migrationID)
	}
	if in.ListenEndpoint == "" {
		t.Error("incoming listen_endpoint is empty")
	}
	if in.AuthToken == "" {
		t.Error("incoming auth_token is empty")
	}

	// Phase 2: source streams out. Stage a deterministic delay so we
	// can observe the pending->running->success arc.
	// Relay the target's distinct NBD endpoint onto the outgoing request,
	// as the CP worker does for live migrations.
	if in.NbdEndpoint == nil || *in.NbdEndpoint == "" {
		t.Fatalf("live incoming nbd_endpoint = %v, want non-empty", in.NbdEndpoint)
	}
	source.AddMigrationResult(vmName, agentmock.MigrationResult{Delay: 80 * time.Millisecond})
	taskID := startOutgoing(t, source, vmName, migrationID, in.ListenEndpoint, in.AuthToken, *in.NbdEndpoint)

	task := pollUntilTerminalURL(t, source, taskID, 2*time.Second)
	if string(task.Status) != "success" {
		t.Fatalf("outgoing task status = %q, want success", task.Status)
	}

	// After terminal-success GetMigration on the source reports
	// completed.
	status, mig := getMigration(t, source, vmName, migrationID)
	if status != http.StatusOK {
		t.Fatalf("get migration status = %d, want 200", status)
	}
	if string(mig.Phase) != "completed" {
		t.Errorf("migration phase = %q, want completed", mig.Phase)
	}
	if mig.ProgressPercent != 100 {
		t.Errorf("migration progress = %d, want 100", mig.ProgressPercent)
	}
	if string(mig.Role) != "source" {
		t.Errorf("migration role = %q, want source", mig.Role)
	}
}

// TestMigration_Failure stages a target_unreachable failure; the
// outgoing task lands failed with the staged error and GetMigration
// reports the failed phase.
func TestMigration_Failure(t *testing.T) {
	source := startMock(t)
	migrationID := uuid.New()
	vmName := "demo"

	source.AddMigrationResult(vmName, agentmock.MigrationResult{
		Delay: 40 * time.Millisecond,
		Error: &agentmock.ErrorEnvelope{
			Code:    "target_unreachable",
			Message: "could not dial target listen endpoint",
		},
	})
	taskID := startOutgoing(t, source, vmName, migrationID, "127.0.0.1:49152", "tok", "127.0.0.1:49153")

	task := pollUntilTerminalURL(t, source, taskID, 2*time.Second)
	if string(task.Status) != "failed" {
		t.Fatalf("outgoing task status = %q, want failed", task.Status)
	}
	if task.Error == nil || task.Error.Code == nil || string(*task.Error.Code) != "target_unreachable" {
		t.Errorf("task error.code = %+v, want target_unreachable", task.Error)
	}

	_, mig := getMigration(t, source, vmName, migrationID)
	if string(mig.Phase) != "failed" {
		t.Errorf("migration phase = %q, want failed", mig.Phase)
	}
}

// TestMigration_Stall stages a result whose phase never reaches
// completed within the poll window — the task stays running and
// GetMigration never advances past active.
func TestMigration_Stall(t *testing.T) {
	source := startMock(t)
	migrationID := uuid.New()
	vmName := "demo"

	source.AddMigrationResult(vmName, agentmock.MigrationResult{
		Delay:      10 * time.Second, // far beyond the poll window
		FinalPhase: "active",         // never converges
	})
	taskID := startOutgoing(t, source, vmName, migrationID, "127.0.0.1:49152", "tok", "127.0.0.1:49153")

	// Brief settle so the task crosses into running.
	time.Sleep(30 * time.Millisecond)
	_, task := getTaskURL(t, source, taskID)
	if s := string(task.Status); s == "success" || s == "failed" || s == "cancelled" {
		t.Fatalf("stalled task reached terminal %q prematurely", s)
	}

	_, mig := getMigration(t, source, vmName, migrationID)
	if string(mig.Phase) == "completed" {
		t.Errorf("stalled migration reached completed; phase = %q", mig.Phase)
	}
}

// TestMigration_Cancel marks the migration record cancelled and
// reflects it through the returned Migration.
func TestMigration_Cancel(t *testing.T) {
	source := startMock(t)
	migrationID := uuid.New()
	vmName := "demo"

	source.AddMigrationResult(vmName, agentmock.MigrationResult{Delay: 10 * time.Second})
	_ = startOutgoing(t, source, vmName, migrationID, "127.0.0.1:49152", "tok", "127.0.0.1:49153")

	mig := cancelMigration(t, source, vmName, migrationID)
	if string(mig.Phase) != "cancelled" {
		t.Errorf("cancelled migration phase = %q, want cancelled", mig.Phase)
	}

	_, after := getMigration(t, source, vmName, migrationID)
	if string(after.Phase) != "cancelled" {
		t.Errorf("phase after cancel = %q, want cancelled", after.Phase)
	}
}

// TestMigration_GetUnknown returns 404 for an unknown migration id.
func TestMigration_GetUnknown(t *testing.T) {
	source := startMock(t)
	status, _ := getMigration(t, source, "demo", uuid.New())
	if status != http.StatusNotFound {
		t.Errorf("get unknown migration status = %d, want 404", status)
	}
}

// postMigrationJSON POSTs an arbitrary body to a migration sub-resource
// path and returns the raw status code. Unlike startIncoming /
// startOutgoing it does not fail-fast on a non-success status, so the
// contract-validation tests can assert the 400 path.
func postMigrationJSON(t *testing.T, m mockURL, vmName, action string, body any) int {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal %s body: %v", action, err)
	}
	url := m.URL() + "/v1/vms/" + vmName + "/migrations/" + action
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("build %s request: %v", action, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", action, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// strPtr returns a pointer to s, for the optional *string identity
// fields on the migration request types.
func strPtr(s string) *string { return &s }

// TestMockIncomingRequiresSourceIdentity asserts the mock rejects an
// incoming request with no SourceNodeIdentity, mirroring a real target
// agent that cannot build its TLS authz pin without it.
func TestMockIncomingRequiresSourceIdentity(t *testing.T) {
	target := startMock(t)
	body := agentapi.MigrationIncomingRequest{
		MigrationID: uuid.New(),
		Mode:        agentapi.MigrationIncomingRequestModeLive,
		// SourceNodeIdentity intentionally omitted.
	}
	if status := postMigrationJSON(t, target, "demo", "incoming", body); status != http.StatusBadRequest {
		t.Errorf("incoming without source_node_identity status = %d, want 400", status)
	}
}

// TestMockIncomingEchoesEndpointAndToken asserts a valid incoming
// request (with SourceNodeIdentity) still yields 200 plus a non-empty
// listen_endpoint and auth_token.
func TestMockIncomingEchoesEndpointAndToken(t *testing.T) {
	target := startMock(t)
	in := startIncoming(t, target, "demo", uuid.New())
	if in.ListenEndpoint == "" {
		t.Error("incoming listen_endpoint is empty")
	}
	if in.AuthToken == "" {
		t.Error("incoming auth_token is empty")
	}
}

// TestMockOutgoingRequiresTargetIdentity asserts the mock rejects an
// outgoing request with no TargetNodeIdentity, mirroring a real source
// agent that cannot set its tls-hostname pin without it.
func TestMockOutgoingRequiresTargetIdentity(t *testing.T) {
	source := startMock(t)
	body := agentapi.MigrationOutgoingRequest{
		AuthToken:      "tok",
		MigrationID:    uuid.New(),
		Mode:           agentapi.MigrationOutgoingRequestMode("live"),
		TargetEndpoint: "127.0.0.1:49152",
		// NbdEndpoint present so the missing-nbd guard is not what trips here.
		NbdEndpoint: strPtr("127.0.0.1:49153"),
		// TargetNodeIdentity intentionally omitted.
	}
	if status := postMigrationJSON(t, source, "demo", "outgoing", body); status != http.StatusBadRequest {
		t.Errorf("outgoing without target_node_identity status = %d, want 400", status)
	}
}

// TestMockIncomingReturnsDistinctNbdEndpointForLive locks the live
// two-endpoint handshake on the target: a live incoming request yields a
// non-empty nbd_endpoint distinct from listen_endpoint (the RAM listener).
// A real target exports the guest disks over a SEPARATE NBD listener.
func TestMockIncomingReturnsDistinctNbdEndpointForLive(t *testing.T) {
	target := startMock(t)
	in := startIncoming(t, target, "demo", uuid.New())
	if in.NbdEndpoint == nil || *in.NbdEndpoint == "" {
		t.Fatalf("live incoming nbd_endpoint = %v, want non-empty", in.NbdEndpoint)
	}
	if *in.NbdEndpoint == in.ListenEndpoint {
		t.Errorf("nbd_endpoint = %q must differ from listen_endpoint %q", *in.NbdEndpoint, in.ListenEndpoint)
	}
}

// TestMockIncomingOmitsNbdEndpointForOffline is the revert-to-confirm: an
// offline incoming request leaves nbd_endpoint nil (offline reuses the
// single endpoint as the qemu-nbd server, no second listener).
func TestMockIncomingOmitsNbdEndpointForOffline(t *testing.T) {
	target := startMock(t)
	body := agentapi.MigrationIncomingRequest{
		MigrationID:        uuid.New(),
		Mode:               agentapi.MigrationIncomingRequestModeOffline,
		SourceNodeIdentity: strPtr("CN=node-source"),
	}
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal offline incoming body: %v", err)
	}
	url := target.URL() + "/v1/vms/demo/migrations/incoming"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("build offline incoming request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("offline incoming: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(resp.Body)
		t.Fatalf("offline incoming status = %d, want 200; body=%s", resp.StatusCode, rb)
	}
	var out agentapi.MigrationIncomingResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode offline incoming response: %v", err)
	}
	if out.NbdEndpoint != nil {
		t.Errorf("offline incoming nbd_endpoint = %q, want nil", *out.NbdEndpoint)
	}
}

// TestMockOutgoingRequiresNbdEndpointForLive is the teeth of the
// outgoing nbd_endpoint relay: a live outgoing request with no
// nbd_endpoint is rejected 400, mirroring a real source agent that
// cannot connect to the target's NBD disk-export listener without it.
// This catches a CP worker that forgets to relay the target's
// nbd_endpoint onto the outgoing request.
func TestMockOutgoingRequiresNbdEndpointForLive(t *testing.T) {
	source := startMock(t)
	body := agentapi.MigrationOutgoingRequest{
		AuthToken:          "tok",
		MigrationID:        uuid.New(),
		Mode:               agentapi.MigrationOutgoingRequestMode("live"),
		TargetEndpoint:     "127.0.0.1:49152",
		TargetNodeIdentity: strPtr("node-target.agents.otherix.local"),
		// NbdEndpoint intentionally omitted.
	}
	if status := postMigrationJSON(t, source, "demo", "outgoing", body); status != http.StatusBadRequest {
		t.Errorf("live outgoing without nbd_endpoint status = %d, want 400", status)
	}
}

// TestMockOutgoingAllowsMissingNbdEndpointForOffline is the
// revert-to-confirm: an offline outgoing request needs NO nbd_endpoint
// (offline reuses the single endpoint), so it still 202s. The live guard
// must not fire for offline.
func TestMockOutgoingAllowsMissingNbdEndpointForOffline(t *testing.T) {
	source := startMock(t)
	body := agentapi.MigrationOutgoingRequest{
		AuthToken:          "tok",
		MigrationID:        uuid.New(),
		Mode:               agentapi.MigrationOutgoingRequestMode("offline"),
		TargetEndpoint:     "127.0.0.1:49152",
		TargetNodeIdentity: strPtr("node-target.agents.otherix.local"),
		// NbdEndpoint omitted - fine for offline.
	}
	if status := postMigrationJSON(t, source, "demo", "outgoing", body); status != http.StatusAccepted {
		t.Errorf("offline outgoing without nbd_endpoint status = %d, want 202", status)
	}
}

// pollUntilTerminalURL is the mockURL-flavoured poll loop (mirrors
// pollUntilTerminal but works off the mockURL interface so it can be
// shared across source/target mocks).
func pollUntilTerminalURL(t *testing.T, m mockURL, taskID uuid.UUID, deadline time.Duration) agentapi.Task {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		status, body := getTaskURL(t, m, taskID)
		if status != http.StatusOK {
			t.Fatalf("tasks.get status = %d, want 200 while polling", status)
		}
		switch string(body.Status) {
		case "success", "failed", "cancelled":
			return body
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach terminal within %v", taskID, deadline)
	return agentapi.Task{}
}

func getTaskURL(t *testing.T, m mockURL, taskID uuid.UUID) (int, agentapi.Task) {
	t.Helper()
	url := m.URL() + "/v1/tasks/" + taskID.String()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build tasks.get request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("tasks.get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, agentapi.Task{}
	}
	var body agentapi.Task
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode Task: %v", err)
	}
	return resp.StatusCode, body
}
