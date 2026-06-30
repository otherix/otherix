// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/node"
)

// runNodeCmd executes the `node` cobra subcommand tree against args, mounting it
// on a throwaway parent that exposes the persistent auth flags the real root
// provides. Mirrors the cluster join-token test harness.
func runNodeCmd(t *testing.T, endpoint string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	parent := node.NewCommand()
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

// joinTokenStubServer returns an httptest server stubbing
// POST /v1/nodes/join-tokens and capturing the request's "kind" field.
func joinTokenStubServer(t *testing.T, gotKind *any, posts *int) *httptest.Server {
	t.Helper()
	id := uuid.NewString()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*posts++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		*gotKind = body["kind"]
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
}

// TestNodeJoinTokenCreate_KindGateway confirms `node join-token create --kind
// gateway` sets kind=gateway on the request, so an operator can mint a gateway
// join token through the same command that mints node tokens.
func TestNodeJoinTokenCreate_KindGateway(t *testing.T) {
	t.Parallel()
	var gotKind any
	var posts int
	srv := joinTokenStubServer(t, &gotKind, &posts)
	defer srv.Close()

	_, _, err := runNodeCmd(t, srv.URL, []string{
		"join-token", "create", "--kind", "gateway", "--ttl", "1h",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1", posts)
	}
	if gotKind != "gateway" {
		t.Errorf("request kind = %v, want gateway", gotKind)
	}
}

// TestNodeJoinTokenCreate_KindDefaultsNode confirms the default omits kind so
// the server applies its node default, preserving existing behaviour.
func TestNodeJoinTokenCreate_KindDefaultsNode(t *testing.T) {
	t.Parallel()
	var gotKind any
	var posts int
	srv := joinTokenStubServer(t, &gotKind, &posts)
	defer srv.Close()

	_, _, err := runNodeCmd(t, srv.URL, []string{
		"join-token", "create", "--ttl", "1h",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotKind != nil {
		t.Errorf("request kind = %v, want absent (nil) so the server defaults to node", gotKind)
	}
}

// TestNodeJoinTokenCreate_KindRejectsCluster confirms the node command refuses
// kind=cluster (cluster tokens are minted via `otherix cluster join-token
// create`) and any other unknown kind, before any request is sent.
func TestNodeJoinTokenCreate_KindRejectsCluster(t *testing.T) {
	t.Parallel()
	var gotKind any
	var posts int
	srv := joinTokenStubServer(t, &gotKind, &posts)
	defer srv.Close()

	_, _, err := runNodeCmd(t, srv.URL, []string{
		"join-token", "create", "--kind", "cluster",
	})
	if err == nil {
		t.Fatal("expected an error for --kind cluster, got nil")
	}
	if posts != 0 {
		t.Errorf("posts = %d, want 0 (rejected before any request)", posts)
	}
}
