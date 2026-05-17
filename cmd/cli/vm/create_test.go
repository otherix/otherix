// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/vm"
)

// runVMCmd mirrors runTemplateCmd в template/create_test.go: mounts
// the `vm` subcommand tree on а throwaway parent с the persistent
// flags the real root provides, then executes args. Returns captured
// stdout / stderr и the cobra error.
func runVMCmd(t *testing.T, endpoint string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	parent := vm.NewCommand()
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

func taskAcceptedJSON(taskID string) []byte {
	return []byte(`{"task_id":"` + taskID + `","status":"pending","links":{"self":"/v1/tasks/` + taskID + `"}}`)
}

// TestVMCreate_CloudInitFile sends --cloud-init=<path> and asserts the
// CP request body carries the resolved YAML in user_data with
// cloud_init_disabled false. Locks in operator UX iteration's primary
// path (file source).
func TestVMCreate_CloudInitFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "ci.yaml")
	ciBody := "#cloud-config\nusers:\n  - name: from-file\n"
	if err := os.WriteFile(yamlPath, []byte(ciBody), 0o600); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}

	var captured map[string]any
	taskID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/vms" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(taskAcceptedJSON(taskID))
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"--name", "vm-ci-file",
		"--template", "ubuntu-jammy",
		"--pool", "pool-mvp",
		"--vcpus", "2",
		"--memory-mb", "512",
		"--cloud-init", yamlPath,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := captured["user_data"]; got != ciBody {
		t.Errorf("user_data = %v, want %q", got, ciBody)
	}
	if disabled, _ := captured["cloud_init_disabled"].(bool); disabled {
		t.Errorf("cloud_init_disabled = true, want false когда --no-cloud-init not set")
	}
}

// TestVMCreate_CloudInitStdin sends --cloud-init=- and pipes content
// through os.Stdin. The cobra command does not let us inject stdin
// directly, but the cloudinit package's stdinReader var is swapped
// in the helper test; here we only verify the CLI passes "-" through
// as а path, and that ReadSource's actual stdin path is covered by
// TestReadSource in the helper package.
func TestVMCreate_CloudInitStdin_FlagAccepted(t *testing.T) {
	t.Parallel()
	// Empty stdin будет parsed as empty body (warning, no error); the
	// CLI sends user_data="" to the server, и the request still goes
	// through. The test asserts the dispatch happened — not the body
	// shape, since the stdin redirection lives in package-level state.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(taskAcceptedJSON(uuid.NewString()))
	}))
	defer srv.Close()

	// Swap stdin к а deterministic, non-blocking buffer so the cobra
	// command does not hang waiting for input on the test runner's
	// terminal. ReadAll on an empty buffer returns nil, []byte{} —
	// Validate emits "empty body" warning, no error, и dispatch
	// proceeds.
	prevStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	_ = w.Close()
	defer func() { os.Stdin = prevStdin }()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"--name", "vm-ci-stdin",
		"--template", "ubuntu-jammy",
		"--pool", "pool-mvp",
		"--vcpus", "1",
		"--memory-mb", "128",
		"--cloud-init", "-",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// TestVMCreate_NoCloudInit sends --no-cloud-init and verifies the
// request body carries cloud_init_disabled=true с user_data absent.
func TestVMCreate_NoCloudInit(t *testing.T) {
	t.Parallel()
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(taskAcceptedJSON(uuid.NewString()))
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"--name", "vm-ci-disabled",
		"--template", "ubuntu-jammy",
		"--pool", "pool-mvp",
		"--vcpus", "2",
		"--memory-mb", "512",
		"--no-cloud-init",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if disabled, _ := captured["cloud_init_disabled"].(bool); !disabled {
		t.Errorf("cloud_init_disabled = %v, want true", captured["cloud_init_disabled"])
	}
	if _, present := captured["user_data"]; present {
		t.Errorf("user_data unexpectedly present in disable-only body: %v", captured["user_data"])
	}
}

// TestVMCreate_CloudInitMutualExclusion locks in the CLI-level guard:
// supplying both --cloud-init AND --no-cloud-init fails before any
// HTTP call leaves the box. DB CHECK + handler validation are the
// server-side backstops; this test covers the operator-friendly UX
// (failure без round-trip).
func TestVMCreate_CloudInitMutualExclusion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "ci.yaml")
	if err := os.WriteFile(yamlPath, []byte("#cloud-config\n"), 0o600); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when mutual-exclusion guard fires")
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"--name", "vm-conflict",
		"--template", "ubuntu-jammy",
		"--pool", "pool-mvp",
		"--vcpus", "1",
		"--memory-mb", "128",
		"--cloud-init", yamlPath,
		"--no-cloud-init",
	})
	if err == nil {
		t.Fatalf("expected mutual-exclusion error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v, want mention of 'mutually exclusive'", err)
	}
}

// TestVMCreate_CloudInitMalformedYAML locks in the validator gating:
// а malformed YAML file is rejected by the CLI before dispatch.
func TestVMCreate_CloudInitMalformedYAML(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "ci.yaml")
	// Unterminated mapping → yaml.v3 parser error.
	if err := os.WriteFile(yamlPath, []byte("#cloud-config\nusers: [unterminated\n"), 0o600); err != nil {
		t.Fatalf("seed yaml: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when YAML validation fails")
	}))
	defer srv.Close()

	_, _, err := runVMCmd(t, srv.URL, []string{
		"create",
		"--name", "vm-bad-yaml",
		"--template", "ubuntu-jammy",
		"--pool", "pool-mvp",
		"--vcpus", "1",
		"--memory-mb", "128",
		"--cloud-init", yamlPath,
	})
	if err == nil {
		t.Fatalf("expected YAML parse error")
	}
	if !strings.Contains(err.Error(), "yaml_parse") {
		t.Errorf("err = %v, want mention of 'yaml_parse'", err)
	}
}
