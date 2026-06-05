// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// runRoot executes the real root command tree against endpoint with a
// throwaway token, returning captured stdout/stderr and the error.
func runRoot(t *testing.T, endpoint string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(append([]string{"--endpoint", endpoint, "--token", "test-token"}, args...))
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetContext(context.Background())
	err = root.Execute()
	return out.String(), errBuf.String(), err
}

// runRootStdin mirrors runRoot but feeds stdin so `create -f -` has a
// deterministic, non-blocking source via cmd.InOrStdin().
func runRootStdin(t *testing.T, endpoint, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(append([]string{"--endpoint", endpoint, "--token", "test-token"}, args...))
	root.SetIn(strings.NewReader(stdin))
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetContext(context.Background())
	err = root.Execute()
	return out.String(), errBuf.String(), err
}

func writeManifest(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return p
}

const createManifest = `apiVersion: otherix/v1
kind: Network
metadata: { name: net-dev }
spec: { type: bridge, bridgeName: br0 }
---
apiVersion: otherix/v1
kind: VM
metadata: { name: web-1 }
spec: { imageURL: https://x/u.qcow2, arch: arm64, network: net-dev }
`

func TestCreateFanOutOrder(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/networks":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"` + uuid.NewString() + `","name":"net-dev","type":"bridge","bridge_name":"br0"}`))
		case "/v1/vms":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"task_id":"` + uuid.NewString() + `","status":"pending","links":{"self":"/v1/tasks/x"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, _, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, createManifest))
	if err != nil {
		t.Fatalf("create -f error = %v", err)
	}
	if len(paths) != 2 || paths[0] != "POST /v1/networks" || paths[1] != "POST /v1/vms" {
		t.Errorf("request order = %v, want [POST /v1/networks, POST /v1/vms]", paths)
	}
	if !bytes.Contains([]byte(stdout), []byte("created")) {
		t.Errorf("stdout = %q, want a created summary", stdout)
	}
}

func TestCreateDryRunIssuesNoCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("dry-run must not issue HTTP calls")
	}))
	defer srv.Close()

	stdout, _, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, createManifest), "--dry-run")
	if err != nil {
		t.Fatalf("dry-run error = %v", err)
	}
	if !bytes.Contains([]byte(stdout), []byte("net-dev")) || !bytes.Contains([]byte(stdout), []byte("web-1")) {
		t.Errorf("dry-run plan = %q, want it to list net-dev and web-1", stdout)
	}
}

func TestCreateRejectsMalformedCloudInit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("no HTTP call must happen when cloud-init is malformed")
	}))
	defer srv.Close()
	const m = `apiVersion: otherix/v1
kind: VM
metadata: { name: web-1 }
spec:
  imageURL: https://x/u.qcow2
  arch: arm64
  cloudInit: |
    #cloud-config
    users: [unterminated
`
	_, _, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, m))
	if err == nil {
		t.Fatalf("expected error for malformed cloud-init")
	}
	if !strings.Contains(err.Error(), "cloud-init") {
		t.Errorf("err = %v, want mention of cloud-init", err)
	}
}

func TestCreateEmptyManifestErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("no HTTP call for an empty manifest")
	}))
	defer srv.Close()
	_, _, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, "# just a comment\n"))
	if err == nil {
		t.Fatalf("expected error for empty manifest")
	}
}

func TestCreateFailureGoesToStderrAndClassified(t *testing.T) {
	// Point at a closed port to force a transport (connection refused) error.
	_, stderr, err := runRoot(t, "http://127.0.0.1:1", "create", "-f", writeManifest(t, createManifest))
	if err == nil {
		t.Fatalf("expected error against unreachable endpoint")
	}
	if !strings.Contains(stderr, "connection_refused") {
		t.Errorf("stderr = %q, want a classified connection_refused: line", stderr)
	}
}

func TestCreateStdinDryRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("dry-run must not call HTTP")
	}))
	defer srv.Close()
	stdout, _, err := runRootStdin(t, srv.URL, createManifest, "create", "-f", "-", "--dry-run")
	if err != nil {
		t.Fatalf("create -f - error = %v", err)
	}
	if !strings.Contains(stdout, "net-dev") || !strings.Contains(stdout, "web-1") {
		t.Errorf("stdin plan = %q, want net-dev and web-1", stdout)
	}
}

func TestCreateMultipleFilesOrderedDryRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("dry-run must not call HTTP")
	}))
	defer srv.Close()
	netFile := writeManifest(t, "apiVersion: otherix/v1\nkind: Network\nmetadata: { name: net-dev }\nspec: { type: bridge, bridgeName: br0 }\n")
	vmFile := writeManifest(t, "apiVersion: otherix/v1\nkind: VM\nmetadata: { name: web-1 }\nspec: { imageURL: https://x/u.qcow2, arch: arm64, network: net-dev }\n")
	// Pass VM file FIRST to prove ordering comes from BuildCreatePlan, not file order.
	stdout, _, err := runRootStdin(t, srv.URL, "", "create", "-f", vmFile, "-f", netFile, "--dry-run")
	if err != nil {
		t.Fatalf("create -f -f error = %v", err)
	}
	// Network must appear before VM in the dry-run plan regardless of -f order.
	ni := strings.Index(stdout, "net-dev")
	vi := strings.Index(stdout, "web-1")
	if ni < 0 || vi < 0 || ni > vi {
		t.Errorf("plan order wrong (network must precede VM):\n%s", stdout)
	}
}

func TestCreatePartialFailureNonZeroExit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/networks" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"already exists"}}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"task_id":"` + uuid.NewString() + `","status":"pending","links":{"self":"/v1/tasks/x"}}`))
	}))
	defer srv.Close()

	_, stderr, err := runRoot(t, srv.URL, "create", "-f", writeManifest(t, createManifest))
	if err == nil {
		t.Fatalf("expected non-nil error when a document fails")
	}
	// The 409 on /v1/networks collapses to ErrNetworkExists, which the
	// fan-out surfaces verbatim as a clean domain message - never wrapped
	// with a transport prefix the way a generic transport error would be.
	if !strings.Contains(stderr, "network name already in use") {
		t.Errorf("stderr = %q, want the verbatim already-exists domain message", stderr)
	}
	if strings.Contains(stderr, "request_failed:") {
		t.Errorf("stderr = %q, a typed conflict must not carry a transport prefix", stderr)
	}
}
