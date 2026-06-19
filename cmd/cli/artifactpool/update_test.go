// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactpool_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/artifactpool"
	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
)

// runArtifactPoolCmd executes the `artifact-pool` cobra subcommand tree against
// args, mounting it on a throwaway parent that exposes the same persistent
// flags the real root provides.
func runArtifactPoolCmd(t *testing.T, endpoint string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	parent := artifactpool.NewCommand()
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

// artifactPoolJSON emits a minimal valid artifactPoolView projection the CLI
// can decode. replicationFactor is embedded as a raw token (an integer or the
// quoted string "all").
func artifactPoolJSON(id, name, replicationFactor string, allNodes bool, nodes []string) []byte {
	membership := map[string]any{"all_nodes": allNodes}
	if len(nodes) > 0 {
		membership["nodes"] = nodes
	}
	body := map[string]any{
		"id":                 id,
		"name":               name,
		"replication_factor": json.RawMessage(replicationFactor),
		"membership":         membership,
		"created_at":         "2026-06-01T10:00:00Z",
		"updated_at":         "2026-06-01T10:05:00Z",
	}
	raw, _ := json.Marshal(body)
	return raw
}

func TestArtifactPoolUpdate_ReplicationFactor(t *testing.T) {
	t.Parallel()
	var patches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		patches++
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		if r.URL.Path != "/v1/artifact-pools/mypool" {
			t.Errorf("path = %s, want /v1/artifact-pools/mypool", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["replication_factor"] != float64(3) {
			t.Errorf("replication_factor = %v, want 3", body["replication_factor"])
		}
		if _, present := body["membership"]; present {
			t.Errorf("membership must be absent when --members not set, got %v", body["membership"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(artifactPoolJSON(uuid.NewString(), "mypool", "3", true, nil))
	}))
	defer srv.Close()

	stdout, _, err := runArtifactPoolCmd(t, srv.URL, []string{
		"update", "mypool", "--replication-factor", "3",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if patches != 1 {
		t.Errorf("patches = %d, want 1", patches)
	}
	if !strings.Contains(stdout, "replication_factor: 3") {
		t.Errorf("stdout missing replication_factor line:\n%s", stdout)
	}
}

func TestArtifactPoolUpdate_Members(t *testing.T) {
	t.Parallel()
	var patches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		patches++
		if r.URL.Path != "/v1/artifact-pools/mypool" {
			t.Errorf("path = %s, want /v1/artifact-pools/mypool", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, present := body["replication_factor"]; present {
			t.Errorf("replication_factor must be absent when not set, got %v", body["replication_factor"])
		}
		m, ok := body["membership"].(map[string]any)
		if !ok {
			t.Fatalf("membership = %v, want object", body["membership"])
		}
		if m["all_nodes"] != false {
			t.Errorf("all_nodes = %v, want false", m["all_nodes"])
		}
		nodes, ok := m["nodes"].([]any)
		if !ok || len(nodes) != 2 || nodes[0] != "node-1" || nodes[1] != "node-2" {
			t.Errorf("nodes = %v, want [node-1 node-2]", m["nodes"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(artifactPoolJSON(uuid.NewString(), "mypool", "1", false, []string{"node-1", "node-2"}))
	}))
	defer srv.Close()

	_, _, err := runArtifactPoolCmd(t, srv.URL, []string{
		"update", "mypool", "--members", "node-1,node-2",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if patches != 1 {
		t.Errorf("patches = %d, want 1", patches)
	}
}

func TestArtifactPoolUpdate_AllAndAll(t *testing.T) {
	t.Parallel()
	var patches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		patches++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["replication_factor"] != "all" {
			t.Errorf("replication_factor = %v, want \"all\"", body["replication_factor"])
		}
		m, ok := body["membership"].(map[string]any)
		if !ok {
			t.Fatalf("membership = %v, want object", body["membership"])
		}
		if m["all_nodes"] != true {
			t.Errorf("all_nodes = %v, want true", m["all_nodes"])
		}
		if _, present := m["nodes"]; present {
			t.Errorf("nodes must be absent for --members all, got %v", m["nodes"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(artifactPoolJSON(uuid.NewString(), "mypool", `"all"`, true, nil))
	}))
	defer srv.Close()

	_, _, err := runArtifactPoolCmd(t, srv.URL, []string{
		"update", "mypool", "--replication-factor", "all", "--members", "all",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if patches != 1 {
		t.Errorf("patches = %d, want 1", patches)
	}
}

func TestArtifactPoolUpdate_NoFlagsErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when no flag is set")
	}))
	defer srv.Close()

	_, _, err := runArtifactPoolCmd(t, srv.URL, []string{"update", "mypool"})
	if err == nil {
		t.Fatalf("expected error when neither --replication-factor nor --members set")
	}
	if !strings.Contains(err.Error(), "--replication-factor") || !strings.Contains(err.Error(), "--members") {
		t.Errorf("err = %v, want mention of both flags", err)
	}
}

func TestArtifactPoolUpdate_InvalidReplicationFactor(t *testing.T) {
	t.Parallel()
	for _, rf := range []string{"0", "xyz"} {
		rf := rf
		t.Run(rf, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				t.Errorf("HTTP call must not happen when --replication-factor is invalid")
			}))
			defer srv.Close()

			_, _, err := runArtifactPoolCmd(t, srv.URL, []string{
				"update", "mypool", "--replication-factor", rf,
			})
			if err == nil {
				t.Fatalf("expected error for --replication-factor %q", rf)
			}
			if !strings.Contains(err.Error(), "replication-factor") {
				t.Errorf("err = %v, want mention of replication-factor", err)
			}
		})
	}
}
