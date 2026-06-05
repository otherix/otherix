// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package pool_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/pool"
)

// runPoolCmd executes the `pool` cobra subcommand tree against args,
// mounting it on a throwaway parent that exposes the same persistent
// flags the real root provides. Returns captured stdout / stderr.
// Parallel of cmd/cli/vm.runVMCmd.
func runPoolCmd(t *testing.T, endpoint string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	parent := pool.NewCommand()
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

// poolJSON emits a minimal valid Pool projection the CLI can decode.
// Mirrors the handlers/storagepools.toView output shape.
func poolJSON(name, node string) []byte {
	body := map[string]any{
		"id":                 uuid.NewString(),
		"node":               node,
		"name":               name,
		"type":               "local_dir",
		"path":               "/opt/otherix/pools/" + name,
		"is_cluster_default": false,
		"config":             map[string]any{},
		"created_at":         "2026-05-15T10:00:00Z",
		"updated_at":         "2026-05-15T10:00:00Z",
	}
	raw, _ := json.Marshal(body)
	return raw
}

// TestPoolCreateRejectsYAMLFormat pins that `yaml` is a get/list-only
// projection format: a mutating command must reject `-o yaml` with a
// usage error and never reach the server, rather than silently rendering
// text. The shared outputFormat helper accepts yaml only when a command
// opts in (get/list), so create must not.
func TestPoolCreateRejectsYAMLFormat(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("create must not reach the server when the output format is invalid")
	}))
	defer srv.Close()
	_, _, err := runPoolCmd(t, srv.URL, []string{
		"create", "p1", "--node", "node-1", "--path", "/opt/p", "--output", "yaml",
	})
	if err == nil {
		t.Fatalf("expected an error for -o yaml on a mutating command")
	}
	if !strings.Contains(err.Error(), "unknown format") {
		t.Errorf("err = %v, want an unknown-format usage error", err)
	}
}

func TestPoolCreate_Happy(t *testing.T) {
	t.Parallel()
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/storage-pools" {
			t.Errorf("path = %s, want /v1/storage-pools", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["name"] != "pool-dev" {
			t.Errorf("name = %v, want pool-dev", body["name"])
		}
		if body["node"] != "node-dev" {
			t.Errorf("node = %v, want node-dev", body["node"])
		}
		if body["type"] != "local_dir" {
			t.Errorf("type = %v, want local_dir (default)", body["type"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(poolJSON("pool-dev", "node-dev"))
	}))
	defer srv.Close()

	stdout, _, err := runPoolCmd(t, srv.URL, []string{
		"create", "pool-dev",
		"--node", "node-dev",
		"--path", "/opt/otherix/pools/default",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1", posts)
	}
	if !strings.Contains(stdout, "pool pool-dev created on node node-dev") {
		t.Errorf("stdout missing creation message: %q", stdout)
	}
}

func TestPoolCreate_ExplicitType(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["type"] != "local_dir" {
			t.Errorf("type = %v, want local_dir", body["type"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(poolJSON("pool-dev", "node-dev"))
	}))
	defer srv.Close()

	_, _, err := runPoolCmd(t, srv.URL, []string{
		"create", "pool-dev",
		"--node", "node-dev",
		"--type", "local_dir",
		"--path", "/opt/otherix/pools/default",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestPoolCreate_MissingNode(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when required flag is missing")
	}))
	defer srv.Close()

	_, _, err := runPoolCmd(t, srv.URL, []string{
		"create", "pool-dev",
		"--path", "/opt/otherix/pools/default",
	})
	if err == nil {
		t.Fatalf("expected error for missing --node")
	}
	if !strings.Contains(err.Error(), "node") {
		t.Errorf("err = %v, want mention of node", err)
	}
}

func TestPoolCreate_MissingPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when required flag is missing")
	}))
	defer srv.Close()

	_, _, err := runPoolCmd(t, srv.URL, []string{
		"create", "pool-dev",
		"--node", "node-dev",
	})
	if err == nil {
		t.Fatalf("expected error for missing --path")
	}
	if !strings.Contains(err.Error(), "path") {
		t.Errorf("err = %v, want mention of path", err)
	}
}

func TestPoolCreate_409Conflict(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"another storage pool with this name already exists on the target node","details":{"field":"name"}}}`))
	}))
	defer srv.Close()

	_, _, err := runPoolCmd(t, srv.URL, []string{
		"create", "pool-dev",
		"--node", "node-dev",
		"--path", "/opt/otherix/pools/default",
	})
	if err == nil {
		t.Fatalf("expected error for 409 conflict")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v, want mention of already exists", err)
	}
	if !strings.Contains(err.Error(), "pool-dev") {
		t.Errorf("err = %v, want mention of pool name", err)
	}
}

func TestPoolCreate_404NodeMissing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"node not found"}}`))
	}))
	defer srv.Close()

	_, _, err := runPoolCmd(t, srv.URL, []string{
		"create", "pool-dev",
		"--node", "missing",
		"--path", "/opt/otherix/pools/default",
	})
	if err == nil {
		t.Fatalf("expected error for 404")
	}
	if !strings.Contains(err.Error(), "not_found") {
		t.Errorf("err = %v, want mention of not_found", err)
	}
}

func TestPoolCreate_OutputJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(poolJSON("pool-dev", "node-dev"))
	}))
	defer srv.Close()

	stdout, _, err := runPoolCmd(t, srv.URL, []string{
		"create", "pool-dev",
		"--node", "node-dev",
		"--path", "/opt/otherix/pools/default",
		"--output", "json",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var obj map[string]any
	if jerr := json.Unmarshal([]byte(stdout), &obj); jerr != nil {
		t.Fatalf("json decode: %v\nstdout=%q", jerr, stdout)
	}
	if obj["name"] != "pool-dev" {
		t.Errorf("name = %v, want pool-dev", obj["name"])
	}
}

func TestPoolCreate_ShowIDs(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(poolJSON("pool-dev", "node-dev"))
	}))
	defer srv.Close()

	stdout, _, err := runPoolCmd(t, srv.URL, []string{
		"create", "pool-dev",
		"--node", "node-dev",
		"--path", "/opt/otherix/pools/default",
		"--show-ids",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(stdout, "id:") {
		t.Errorf("stdout missing id line when --show-ids: %q", stdout)
	}
}
