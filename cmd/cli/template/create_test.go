// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package template_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/template"
)

// runTemplateCmd executes the `template` cobra subcommand tree
// against args, mounting it on a throwaway parent that exposes the
// same persistent flags the real root provides. Returns captured
// stdout / stderr.
func runTemplateCmd(t *testing.T, endpoint string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	parent := template.NewCommand()
	parent.PersistentFlags().String(cliauth.FlagConfig, "", "")
	parent.PersistentFlags().String(cliauth.FlagEndpoint, "", "")
	parent.PersistentFlags().String(cliauth.FlagToken, "", "")
	parent.PersistentFlags().String(cliauth.FlagCluster, "", "")

	full := append([]string{"--endpoint", endpoint, "--token", "test-token"}, args...)
	parent.SetArgs(full)
	parent.SilenceUsage = true
	parent.SilenceErrors = true
	var out, errBuf bytes.Buffer
	parent.SetOut(&out)
	parent.SetErr(&errBuf)
	parent.SetContext(context.Background())
	err = parent.Execute()
	return out.String(), errBuf.String(), err
}

// templateJSONComputeMode mirrors templateJSON but emits null for
// `image_checksum_sha256` — the wire shape а compute-mode template
// carries before its first successful materialisation.
func templateJSONComputeMode(name string) []byte {
	body := map[string]any{
		"id": uuid.NewString(), "owner_id": uuid.NewString(),
		"name": name, "description": "", "visibility": "private",
		"architecture": "arm64", "os_family": "linux", "os_variant": "",
		"image_url":             "https://example.com/img.qcow2",
		"image_checksum_sha256": nil,
		"image_format":          "qcow2", "image_size_bytes": nil,
		"firmware_type": "uefi", "firmware_id": nil,
		"default_cpu_cores": 2, "default_memory_mib": 2048, "default_disk_gib": 20,
		"cloud_init_supported": true, "qemu_guest_agent_expected": false,
		"metadata":   map[string]any{},
		"created_at": "2026-05-13T10:00:00Z", "updated_at": "2026-05-13T10:00:00Z",
	}
	raw, _ := json.Marshal(body)
	return raw
}

// templateJSON helps tests emit а minimal valid Template projection
// the CLI can decode. Mirrors handlers/templates.toView output.
func templateJSON(name string) []byte {
	body := map[string]any{
		"id": uuid.NewString(), "owner_id": uuid.NewString(),
		"name": name, "description": "", "visibility": "private",
		"architecture": "arm64", "os_family": "linux", "os_variant": "",
		"image_url":             "https://example.com/img.qcow2",
		"image_checksum_sha256": strings.Repeat("a", 64),
		"image_format":          "qcow2", "image_size_bytes": nil,
		"firmware_type": "uefi", "firmware_id": nil,
		"default_cpu_cores": 2, "default_memory_mib": 2048, "default_disk_gib": 20,
		"cloud_init_supported": true, "qemu_guest_agent_expected": false,
		"metadata":   map[string]any{},
		"created_at": "2026-05-13T10:00:00Z", "updated_at": "2026-05-13T10:00:00Z",
	}
	raw, _ := json.Marshal(body)
	return raw
}

func TestTemplateCreate_RegistrationOnly(t *testing.T) {
	t.Parallel()
	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/templates" {
			t.Errorf("path = %s, want /v1/templates", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(templateJSON("test-noimage"))
	}))
	defer srv.Close()

	stdout, _, err := runTemplateCmd(t, srv.URL, []string{
		"create", "test-noimage",
		"--arch", "arm64",
		"--os-family", "linux",
		"--image-url", "https://example.com/img.qcow2",
		"--expected-sha256", strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if atomic.LoadInt32(&posts) != 1 {
		t.Errorf("posts = %d, want 1 (registration only — no materialise call without --pool)", posts)
	}
	if !strings.Contains(stdout, "template test-noimage created") {
		t.Errorf("stdout missing creation message: %q", stdout)
	}
	if strings.Contains(stdout, "materialise") {
		t.Errorf("stdout contained materialise output without --pool: %q", stdout)
	}
}

func TestTemplateCreate_HappyWithPoolNoWait(t *testing.T) {
	t.Parallel()
	var createCalls, importCalls int32
	taskID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/templates":
			atomic.AddInt32(&createCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(templateJSON("ubuntu-jammy"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/templates/ubuntu-jammy/images":
			atomic.AddInt32(&importCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"` + taskID + `","status":"pending","links":{"self":"/v1/tasks/` + taskID + `"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	stdout, _, err := runTemplateCmd(t, srv.URL, []string{
		"create", "ubuntu-jammy",
		"--arch", "arm64",
		"--os-family", "linux",
		"--image-url", "https://example.com/img.qcow2",
		"--expected-sha256", strings.Repeat("a", 64),
		"--pool", "pool-mvp",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if atomic.LoadInt32(&createCalls) != 1 {
		t.Errorf("createCalls = %d, want 1", createCalls)
	}
	if atomic.LoadInt32(&importCalls) != 1 {
		t.Errorf("importCalls = %d, want 1", importCalls)
	}
	if !strings.Contains(stdout, "materialise accepted task="+taskID) {
		t.Errorf("stdout missing materialise accepted line: %q", stdout)
	}
	if strings.Contains(stdout, "image materialised") {
		t.Errorf("stdout should not contain terminal-success message без --wait: %q", stdout)
	}
}

func TestTemplateCreate_IdempotentFallThrough(t *testing.T) {
	t.Parallel()
	// First POST /v1/templates returns 409; CLI must fall through to
	// GET /v1/templates/{name} (к surface а usable Template view) и
	// then POST /v1/templates/{name}/images. Verifies idempotent retry.
	var createCalls, getCalls, importCalls int32
	taskID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/templates":
			atomic.AddInt32(&createCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"template name already exists"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/templates/ubuntu-jammy":
			atomic.AddInt32(&getCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(templateJSON("ubuntu-jammy"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/templates/ubuntu-jammy/images":
			atomic.AddInt32(&importCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"` + taskID + `","status":"pending","links":{"self":"/v1/tasks/` + taskID + `"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	stdout, _, err := runTemplateCmd(t, srv.URL, []string{
		"create", "ubuntu-jammy",
		"--arch", "arm64",
		"--os-family", "linux",
		"--image-url", "https://example.com/img.qcow2",
		"--expected-sha256", strings.Repeat("a", 64),
		"--pool", "pool-mvp",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if atomic.LoadInt32(&createCalls) != 1 {
		t.Errorf("createCalls = %d, want 1", createCalls)
	}
	if atomic.LoadInt32(&getCalls) != 1 {
		t.Errorf("getCalls = %d, want 1 (fall-through к fetch existing template)", getCalls)
	}
	if atomic.LoadInt32(&importCalls) != 1 {
		t.Errorf("importCalls = %d, want 1", importCalls)
	}
	if !strings.Contains(stdout, "already exists; proceeding к materialisation") {
		t.Errorf("stdout missing fall-through message: %q", stdout)
	}
}

func TestTemplateCreate_MaterialiseRejected(t *testing.T) {
	t.Parallel()
	// Registration succeeds, materialise returns 404 — confirm the
	// CLI surfaces а retry-friendly message и exits non-zero.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/templates":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(templateJSON("ubuntu-jammy"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/templates/ubuntu-jammy/images":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"storage pool not found"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	stdout, _, err := runTemplateCmd(t, srv.URL, []string{
		"create", "ubuntu-jammy",
		"--arch", "arm64",
		"--os-family", "linux",
		"--image-url", "https://example.com/img.qcow2",
		"--expected-sha256", strings.Repeat("a", 64),
		"--pool", "missing-pool",
	})
	if err == nil {
		t.Fatalf("expected error для materialise failure")
	}
	if !strings.Contains(err.Error(), "remains registered") {
		t.Errorf("err missing retry hint: %v", err)
	}
	if !strings.Contains(stdout, "template ubuntu-jammy created") {
		t.Errorf("stdout missing registration success line: %q", stdout)
	}
}

func TestTemplateCreate_WaitWithoutPoolErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when --wait validation fails")
	}))
	defer srv.Close()

	_, _, err := runTemplateCmd(t, srv.URL, []string{
		"create", "x",
		"--arch", "arm64",
		"--os-family", "linux",
		"--image-url", "https://example.com/img.qcow2",
		"--expected-sha256", strings.Repeat("a", 64),
		"--wait",
	})
	if err == nil {
		t.Fatalf("expected error для --wait without --pool")
	}
	if !strings.Contains(err.Error(), "--wait requires --pool") {
		t.Errorf("err = %v, want '--wait requires --pool'", err)
	}
}

func TestTemplateCreate_MissingRequiredFlag(t *testing.T) {
	t.Parallel()
	// --expected-sha256 is not in the required set; the remaining
	// required identity flags are --arch / --os-family / --image-url.
	// Cobra enforces them up-front, so HTTP must never fire when one
	// of them is omitted.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when required flag is missing")
	}))
	defer srv.Close()

	_, _, err := runTemplateCmd(t, srv.URL, []string{
		"create", "x",
		// --arch omitted on purpose
		"--os-family", "linux",
		"--image-url", "https://example.com/img.qcow2",
	})
	if err == nil {
		t.Fatalf("expected error для missing --arch")
	}
	if !strings.Contains(err.Error(), "arch") {
		t.Errorf("err = %v, want mention of arch", err)
	}
}

// TestTemplateCreate_ArchitectureFlagRejected locks in the clean
// rename: the old `--architecture` flag is gone, no alias. Cobra
// surfaces it as an unknown-flag error.
func TestTemplateCreate_ArchitectureFlagRejected(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when an unknown flag is supplied")
	}))
	defer srv.Close()

	_, _, err := runTemplateCmd(t, srv.URL, []string{
		"create", "x",
		"--architecture", "arm64", // deliberately the old, retired flag
		"--os-family", "linux",
		"--image-url", "https://example.com/img.qcow2",
	})
	if err == nil {
		t.Fatalf("expected error для retired --architecture flag")
	}
	if !strings.Contains(err.Error(), "unknown flag: --architecture") {
		t.Errorf("err = %v, want 'unknown flag: --architecture'", err)
	}
}

// TestTemplateCreate_ComputeMode locks in compute mode: omitting
// --expected-sha256 produces а request body without (or с nil)
// `image_checksum_sha256`, and the CP-side compute-mode flow proceeds
// normally (registration succeeds; the back-propagation is observable
// in а full integration test, not at the CLI handler-test layer).
func TestTemplateCreate_ComputeMode(t *testing.T) {
	t.Parallel()
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(templateJSONComputeMode("ubuntu-compute"))
	}))
	defer srv.Close()

	stdout, _, err := runTemplateCmd(t, srv.URL, []string{
		"create", "ubuntu-compute",
		"--arch", "arm64",
		"--os-family", "linux",
		"--image-url", "https://example.com/img.qcow2",
		// --expected-sha256 deliberately omitted
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// The CreateTemplateRequest's ImageChecksumSHA256 is а plain
	// string в the CLI wire shape, so omitting the flag results in
	// an empty value. The CP treats empty as absent (compute mode).
	got, present := captured["image_checksum_sha256"]
	switch {
	case !present:
		// Acceptable: encoder omitted the field altogether.
	case got == "":
		// Acceptable: encoder emitted "".
	default:
		t.Errorf("image_checksum_sha256 = %v, want absent or empty (compute mode)", got)
	}
	if !strings.Contains(stdout, "template ubuntu-compute created") {
		t.Errorf("stdout missing creation message: %q", stdout)
	}
}

func TestTemplateCreate_OutputJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(templateJSON("ubuntu-jammy"))
	}))
	defer srv.Close()

	stdout, _, err := runTemplateCmd(t, srv.URL, []string{
		"create", "ubuntu-jammy",
		"--arch", "arm64",
		"--os-family", "linux",
		"--image-url", "https://example.com/img.qcow2",
		"--expected-sha256", strings.Repeat("a", 64),
		"--output", "json",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Output: "<created line>\n<json...>" — strip lead line and decode.
	lines := strings.SplitN(stdout, "\n", 2)
	if len(lines) < 2 {
		t.Fatalf("stdout malformed: %q", stdout)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &obj); err != nil {
		t.Fatalf("json decode: %v\nstdout=%q", err, stdout)
	}
	if obj["name"] != "ubuntu-jammy" {
		t.Errorf("name = %v, want ubuntu-jammy", obj["name"])
	}
}
