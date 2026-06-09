// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cluster_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/cluster"
	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
)

// runClusterCmd executes the `cluster` cobra subcommand tree against
// args, mounting it on a throwaway parent that exposes the same
// persistent flags the real root provides. Mirrors
// network_test.runNetworkCmd's auth-injection pattern.
func runClusterCmd(t *testing.T, endpoint string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	parent := cluster.NewCommand()
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

// TestClusterJoinTokenCreate_Happy confirms `cluster join-token create`
// forces kind=cluster on the request body and prints the token bundle
// plus a ready-to-paste `otherix-api join` hint.
func TestClusterJoinTokenCreate_Happy(t *testing.T) {
	t.Parallel()
	var posts int
	var gotKind any
	id := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/nodes/join-tokens" {
			t.Errorf("path = %s, want /v1/nodes/join-tokens", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		gotKind = body["kind"]
		resp := map[string]any{
			"id":                    id,
			"intended_node_name":    nil,
			"expires_at":            "2026-06-01T11:00:00Z",
			"max_uses":              1,
			"consumption_count":     0,
			"created_by_user_id":    nil,
			"created_at":            "2026-06-01T10:00:00Z",
			"token":                 "otx_join_TESTPLAINTEXT",
			"ca_fingerprint_sha256": "abc123def456",
		}
		raw, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	stdout, _, err := runClusterCmd(t, srv.URL, []string{
		"join-token", "create", "--ttl", "1h",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1", posts)
	}
	if gotKind != "cluster" {
		t.Errorf("request kind = %v, want cluster", gotKind)
	}
	if !strings.Contains(stdout, "otx_join_TESTPLAINTEXT") {
		t.Errorf("stdout missing token plaintext:\n%s", stdout)
	}
	if !strings.Contains(stdout, "abc123def456") {
		t.Errorf("stdout missing CA fingerprint:\n%s", stdout)
	}
	if !strings.Contains(stdout, "otherix-api join") {
		t.Errorf("stdout missing `otherix-api join` hint:\n%s", stdout)
	}
}
