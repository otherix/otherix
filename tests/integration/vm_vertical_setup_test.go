// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// vmCreateBody is the wire-shape body POST /v1/vms accepts (name-based
// references for template + pool). Mirrors
// cmd/cli/internal/cpclient.CreateVMRequest; re-declared here so the
// integration package does not import the CLI package. The `template`
// and `pool` fields accept either UUID strings or names — the
// integration suite passes UUID strings throughout for determinism.
type vmCreateBody struct {
	Name              string  `json:"name"`
	Template          string  `json:"template"`
	Pool              string  `json:"pool,omitempty"`
	Node              *string `json:"node,omitempty"`
	VCPUs             int     `json:"vcpus"`
	MemoryMB          int     `json:"memory_mb"`
	UserData          *string `json:"user_data,omitempty"`
	CloudInitDisabled bool    `json:"cloud_init_disabled,omitempty"`
}

// createVM POSTs /v1/vms, asserts 202, and returns the decoded task uuid.
// idemKey is the optional Idempotency-Key header — empty omits it.
func (v *verticalSlice) createVM(t *testing.T, ctx context.Context, body vmCreateBody, idemKey string) (uuid.UUID, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal vm create body: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.cpServer.URL+"/v1/vms", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new vm create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+v.adminToken)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("vm create POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read vm create body: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("vm create status = %d, body = %s, want 202", resp.StatusCode, respBody)
	}
	var accepted struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(respBody, &accepted); err != nil {
		t.Fatalf("decode AsyncTaskAccepted: %v", err)
	}
	id, err := uuid.Parse(accepted.TaskID)
	if err != nil {
		t.Fatalf("task_id %q is not a uuid: %v", accepted.TaskID, err)
	}
	return id, respBody
}

// createVMRaw issues the POST without asserting success — useful for
// negative tests (RBAC denials, validation failures). Returns the raw
// status code and body so the caller decodes the envelope itself.
func (v *verticalSlice) createVMRaw(t *testing.T, ctx context.Context, body vmCreateBody, token, idemKey string) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal vm create body: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.cpServer.URL+"/v1/vms", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new vm create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("vm create POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read vm create body: %v", err)
	}
	return resp.StatusCode, respBody
}

// deleteVMRequest issues a DELETE /v1/vms/{name} with the supplied
// auth token. The path is name-only — the helper resolves the
// caller's vmID to its name via the store before building the URL
// so test fixtures keep their UUID-based addressing at the seam
// where they have it (extractVMIDFromTask).
func (v *verticalSlice) deleteVMRequest(t *testing.T, ctx context.Context, vmID uuid.UUID, token, idemKey string) (int, []byte) {
	t.Helper()
	row, err := v.store.Queries().GetVMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("resolve vm name for %s: %v", vmID, err)
	}
	url := v.cpServer.URL + "/v1/vms/" + row.Name
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("new vm delete request: %v", err)
	}
	if token == "" {
		token = v.adminToken
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("vm delete: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read vm delete body: %v", err)
	}
	return resp.StatusCode, body
}

// postVMLifecycleRequest issues POST /v1/vms/{name}/<action> with the
// supplied auth token. Used by the L1 sync ops integration tests —
// the action segment must be one of `pause`, `resume`, `reset`.
// Returns raw status / body so callers can decode either the success
// VM view or an APIError envelope.
func (v *verticalSlice) postVMLifecycleRequest(t *testing.T, ctx context.Context, vmID uuid.UUID, action, token string) (int, []byte) {
	t.Helper()
	row, err := v.store.Queries().GetVMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("resolve vm name for %s: %v", vmID, err)
	}
	url := v.cpServer.URL + "/v1/vms/" + row.Name + "/" + action
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("new vm %s request: %v", action, err)
	}
	if token == "" {
		token = v.adminToken
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("vm %s: %v", action, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read vm %s body: %v", action, err)
	}
	return resp.StatusCode, body
}

// getVMRequest issues GET /v1/vms/{name} with the supplied token (admin
// when empty). Returns raw status / body — caller decodes. The
// helper resolves vmID to its name before constructing the URL
// (mirrors deleteVMRequest).
func (v *verticalSlice) getVMRequest(t *testing.T, ctx context.Context, vmID uuid.UUID, token string) (int, []byte) {
	t.Helper()
	row, err := v.store.Queries().GetVMByID(ctx, vmID)
	if err != nil {
		t.Fatalf("resolve vm name for %s: %v", vmID, err)
	}
	url := v.cpServer.URL + "/v1/vms/" + row.Name
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new vm get request: %v", err)
	}
	if token == "" {
		token = v.adminToken
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("vm get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read vm get body: %v", err)
	}
	return resp.StatusCode, body
}

// listVMsRequest issues GET /v1/vms?... with the supplied query string
// suffix (e.g. "limit=2&pool_id=..."). Empty suffix issues a no-args
// list.
func (v *verticalSlice) listVMsRequest(t *testing.T, ctx context.Context, query string) (int, []byte) {
	t.Helper()
	url := v.cpServer.URL + "/v1/vms"
	if query != "" {
		url += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new vm list request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+v.adminToken)
	resp, err := v.cpServer.Client().Do(req)
	if err != nil {
		t.Fatalf("vm list: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read vm list body: %v", err)
	}
	return resp.StatusCode, body
}

// awaitVMCreateEvent drains the river completions channel until a
// vm.create event surfaces (or the deadline fires). Returns the
// event so callers can branch on Kind / state.
func (v *verticalSlice) awaitVMCreateEvent(t *testing.T, deadline time.Duration) *river.Event {
	t.Helper()
	timer := time.After(deadline)
	for {
		select {
		case ev := <-v.completions:
			if ev.Job.Kind == "vm.create" {
				return ev
			}
		case <-timer:
			t.Fatalf("vm.create event did not arrive within %v", deadline)
		}
	}
}

// awaitVMDeleteEvent drains the river completions channel until a
// vm.delete event surfaces (or the deadline fires).
func (v *verticalSlice) awaitVMDeleteEvent(t *testing.T, deadline time.Duration) *river.Event {
	t.Helper()
	timer := time.After(deadline)
	for {
		select {
		case ev := <-v.completions:
			if ev.Job.Kind == "vm.delete" {
				return ev
			}
		case <-timer:
			t.Fatalf("vm.delete event did not arrive within %v", deadline)
		}
	}
}

// agentVMCreateCallCount returns how many times the mock-agent has
// observed POST /v1/vms — used by idempotency / resumption tests
// to assert the CP did not double-call the agent.
func (v *verticalSlice) agentVMCreateCallCount() int {
	return v.countOperation("vms.create")
}

// agentVMDeleteCallCount returns how many times the mock-agent has
// observed DELETE /v1/vms/{id}.
func (v *verticalSlice) agentVMDeleteCallCount() int {
	return v.countOperation("vms.delete")
}

// loginUser is a generic login helper for non-admin role tests.
// loginAdmin only knows the admin token; this lets RBAC tests acquire
// developer / viewer JWTs.
func loginUser(t *testing.T, cpURL, email, password string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}
	resp, err := http.Post(cpURL+"/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("login status = %d, body = %s, want 200", resp.StatusCode, raw)
	}
	var login struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	return login.AccessToken
}

// seedUser inserts a non-admin user with the given role and returns
// (id, email, password). Mirrors seedAdmin for the RBAC tests that
// need developer / viewer principals.
func seedUser(t *testing.T, ctx context.Context, s *store.Store, role string) (uuid.UUID, string, string) {
	t.Helper()
	id := uuid.New()
	email := fmt.Sprintf("vertical-%s-%s@example.test", role, uuid.NewString()[:8])
	password := "correct-horse-battery-staple"

	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := s.Queries().CreateUser(ctx, store.CreateUserParams{
		ID:           id,
		Email:        email,
		PasswordHash: hash,
		DisplayName:  "vertical " + role,
		Role:         role,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return id, email, password
}

// seedTemplateForVM seeds a template + agentmock-staged image for use
// by vm.create. Future behaviour will require a storage_images row;
// the MVP agent reads the file directly, so the integration path
// stages a pre-existing CachedImage on the agentmock side and uses
// the matching checksum in the template row.
//
// Returns the template plus the hex checksum the test will pass via
// the API request body's template_id (the request body carries
// template_id only; the worker derives the checksum from the row).
func seedTemplateForVM(t *testing.T, ctx context.Context, v *verticalSlice, ownerID uuid.UUID, name string, checksumByte byte, visibility string) store.Template {
	t.Helper()
	tpl := seedTemplate(t, ctx, v.store, ownerID, name, checksumByte)
	if visibility == "public" {
		if _, err := v.store.Queries().SetTemplateVisibility(ctx, store.SetTemplateVisibilityParams{
			ID:         tpl.ID,
			Visibility: "public",
		}); err != nil {
			t.Fatalf("SetTemplateVisibility public: %v", err)
		}
		// Re-load after the visibility flip.
		tpl, _ = v.store.Queries().GetTemplate(ctx, tpl.ID)
	}

	// Stage a corresponding cached-image entry on the agentmock so the
	// vm.create executor's downstream agent observes a "template
	// present" world. The agentmock's vm.create handler does NOT verify
	// template presence (vms.go is currently permissive); this is here
	// for completeness and for future drift protection.
	checksum := hexChecksum(tpl.ImageChecksumSha256)
	_ = v.mock.AddImage(v.pool.Name, agentmock.CachedImage{
		ChecksumSHA256: checksum,
		Format:         "qcow2",
		SizeBytes:      1 << 20,
		Path:           "/opt/otherix/pools/test/templates/" + checksum + ".qcow2",
	})

	return tpl
}

// stageVMCreateSuccess pre-loads the agentmock with a successful
// VMCreateResult against the supplied vm name. Per Pre-L1 Path D the
// mock's vm-create result queue is keyed by name (was UUID); callers
// pass the same `Name` they wrote into the `vmCreateBody`. Tests that
// want terminal-failed staging use stageVMCreateFailure instead.
func stageVMCreateSuccess(v *verticalSlice, vmName string, delay time.Duration) {
	v.mock.AddVMCreateResult(vmName, agentmock.VMCreateResult{
		Status: "success",
		Delay:  delay,
	})
}

// stageVMCreateFailure pre-loads the agentmock with a terminal-failed
// outcome carrying the supplied agent error code.
func stageVMCreateFailure(v *verticalSlice, vmName, code, message string, delay time.Duration) {
	v.mock.AddVMCreateResult(vmName, agentmock.VMCreateResult{
		Status: "failed",
		Error:  &agentmock.ErrorEnvelope{Code: code, Message: message},
		Delay:  delay,
	})
}

// stageVMDeleteSuccess pre-loads the agentmock with a successful
// VMDeleteResult against the supplied vm name.
func stageVMDeleteSuccess(v *verticalSlice, vmName string, delay time.Duration) {
	v.mock.AddVMDeleteResult(vmName, agentmock.VMDeleteResult{
		Status: "success",
		Delay:  delay,
	})
}

// stageVMDeleteFailure pre-loads the agentmock with a terminal-failed
// outcome.
func stageVMDeleteFailure(v *verticalSlice, vmName, code, message string, delay time.Duration) {
	v.mock.AddVMDeleteResult(vmName, agentmock.VMDeleteResult{
		Status: "failed",
		Error:  &agentmock.ErrorEnvelope{Code: code, Message: message},
		Delay:  delay,
	})
}

// extractVMIDFromTask reads the task row, parses task.result, and
// returns the vm_id the worker projected. Helper because every vm
// test validates this projection at the tail.
func extractVMIDFromTask(t *testing.T, row store.Task) uuid.UUID {
	t.Helper()
	if row.ResourceID == nil {
		t.Fatalf("task.resource_id is nil")
	}
	return *row.ResourceID
}

// jsonContains is a tiny matcher that returns true when haystack
// (raw JSON bytes) contains the needle substring. Used for snapshot-
// style assertions where a typed decode would be overkill.
func jsonContains(haystack []byte, needle string) bool {
	return strings.Contains(string(haystack), needle)
}
