// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package network_test

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
	"github.com/otherix/otherix/cmd/cli/network"
)

// runNetworkCmd executes the `network` cobra subcommand tree against
// args, mounting it on a throwaway parent that exposes the same
// persistent flags the real root provides. Parallel of
// cmd/cli/pool_test.runPoolCmd.
func runNetworkCmd(t *testing.T, endpoint string, args []string) (stdout, stderr string, err error) {
	t.Helper()
	parent := network.NewCommand()
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

// networkJSON emits a minimal valid networkView projection the CLI can
// decode. Mirrors handlers/networks.toView output.
func networkJSON(id, name string) []byte {
	body := map[string]any{
		"id":          id,
		"name":        name,
		"type":        "bridge",
		"bridge_name": "br0",
		"managed":     false,
		"egress":      "none",
		"vlan_tag":    nil,
		"mtu":         1500,
		"subnet":      nil,
		"gateway":     nil,
		"config":      map[string]any{},
		"created_at":  "2026-06-01T10:00:00Z",
		"updated_at":  "2026-06-01T10:00:00Z",
	}
	raw, _ := json.Marshal(body)
	return raw
}

func TestNetworkCreate_Happy(t *testing.T) {
	t.Parallel()
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/networks" {
			t.Errorf("path = %s, want /v1/networks", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["name"] != "net-mvp" {
			t.Errorf("name = %v, want net-mvp", body["name"])
		}
		if body["bridge_name"] != "br0" {
			t.Errorf("bridge_name = %v, want br0", body["bridge_name"])
		}
		if body["type"] != "bridge" {
			t.Errorf("type = %v, want bridge (default)", body["type"])
		}
		if _, present := body["egress"]; present {
			t.Errorf("egress should be omitted (default none), got %v", body["egress"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(networkJSON(uuid.NewString(), "net-mvp"))
	}))
	defer srv.Close()

	stdout, _, err := runNetworkCmd(t, srv.URL, []string{
		"create", "net-mvp", "--bridge-name", "br0",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1", posts)
	}
	if !strings.Contains(stdout, "network net-mvp created") {
		t.Errorf("stdout missing creation message: %q", stdout)
	}
}

func TestNetworkCreate_MissingBridgeName(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when required flag is missing")
	}))
	defer srv.Close()

	_, _, err := runNetworkCmd(t, srv.URL, []string{"create", "net-mvp"})
	if err == nil {
		t.Fatalf("expected error for missing --bridge-name")
	}
	if !strings.Contains(err.Error(), "bridge-name") {
		t.Errorf("err = %v, want mention of bridge-name", err)
	}
}

func TestNetworkCreate_NatBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["managed"] != true {
			t.Errorf("managed = %v, want true", body["managed"])
		}
		if body["egress"] != "nat" {
			t.Errorf("egress = %v, want nat", body["egress"])
		}
		if body["subnet"] != "10.10.0.0/24" {
			t.Errorf("subnet = %v, want 10.10.0.0/24", body["subnet"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(networkJSON(uuid.NewString(), "net-nat"))
	}))
	defer srv.Close()

	_, _, err := runNetworkCmd(t, srv.URL, []string{
		"create", "net-nat",
		"--bridge-name", "br-nat",
		"--managed",
		"--egress", "nat",
		"--subnet", "10.10.0.0/24",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestNetworkCreate_409Conflict(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict","message":"network name already in use"}}`))
	}))
	defer srv.Close()

	_, _, err := runNetworkCmd(t, srv.URL, []string{
		"create", "net-mvp", "--bridge-name", "br0",
	})
	if err == nil {
		t.Fatalf("expected error for 409 conflict")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v, want mention of already exists", err)
	}
}

func TestNetworkCreate_400Validation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"validation_failed","message":"nat egress requires a managed network"}}`))
	}))
	defer srv.Close()

	_, _, err := runNetworkCmd(t, srv.URL, []string{
		"create", "net-bad", "--bridge-name", "br0", "--egress", "nat",
	})
	if err == nil {
		t.Fatalf("expected error for 400 validation")
	}
	if !strings.Contains(err.Error(), "validation_failed") {
		t.Errorf("err = %v, want mention of validation_failed", err)
	}
}

func TestNetworkList_Table(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/networks" {
			t.Errorf("path = %s, want /v1/networks", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[` + string(networkJSON(uuid.NewString(), "net-a")) +
			`,` + string(networkJSON(uuid.NewString(), "net-b")) + `],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	stdout, _, err := runNetworkCmd(t, srv.URL, []string{"list"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, want := range []string{"NAME", "TYPE", "BRIDGE", "MANAGED", "EGRESS", "MTU", "net-a", "net-b"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table missing %q:\n%s", want, stdout)
		}
	}
}

func TestNetworkGet_TextWithStatus(t *testing.T) {
	t.Parallel()
	id := uuid.NewString()
	nodeID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/networks/"+id {
			t.Errorf("path = %s, want /v1/networks/%s", r.URL.Path, id)
		}
		body := map[string]any{
			"id": id, "name": "net-mvp", "type": "bridge", "bridge_name": "br0",
			"managed": false, "egress": "none", "vlan_tag": nil, "mtu": 1500,
			"subnet": nil, "gateway": nil, "config": map[string]any{},
			"created_at": "2026-06-01T10:00:00Z", "updated_at": "2026-06-01T10:00:00Z",
			"status": map[string]any{
				"nodes": []map[string]any{
					{"node_id": nodeID, "node_name": "node-a", "reconciliation_status": "ready", "reconciliation_error": nil, "last_reconciled_at": "2026-06-01T10:05:00Z"},
				},
			},
		}
		raw, _ := json.Marshal(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	stdout, _, err := runNetworkCmd(t, srv.URL, []string{"get", id})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// The NODE column renders the resolved node name, not its uuid.
	for _, want := range []string{"status:", "NODE", "STATUS", "ERROR", "node-a", "ready"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, nodeID) {
		t.Errorf("output should render node name, not uuid %q:\n%s", nodeID, stdout)
	}
}

// TestNetworkGet_StatusEmptyNodeNameFallsBackToUUID confirms that when the
// server reports a status row with an empty node_name (the node was deleted
// but a stale status lingers), the NODE column falls back to the node uuid so
// the operator still has a stable handle.
func TestNetworkGet_StatusEmptyNodeNameFallsBackToUUID(t *testing.T) {
	t.Parallel()
	id := uuid.NewString()
	nodeID := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"id": id, "name": "net-mvp", "type": "bridge", "bridge_name": "br0",
			"managed": false, "egress": "none", "vlan_tag": nil, "mtu": 1500,
			"subnet": nil, "gateway": nil, "config": map[string]any{},
			"created_at": "2026-06-01T10:00:00Z", "updated_at": "2026-06-01T10:00:00Z",
			"status": map[string]any{
				"nodes": []map[string]any{
					{"node_id": nodeID, "node_name": "", "reconciliation_status": "pending", "reconciliation_error": nil, "last_reconciled_at": nil},
				},
			},
		}
		raw, _ := json.Marshal(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	stdout, _, err := runNetworkCmd(t, srv.URL, []string{"get", id})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(stdout, nodeID) {
		t.Errorf("output should fall back to node uuid %q when name empty:\n%s", nodeID, stdout)
	}
}

// TestNetworkGet_NameResolves exercises the client-side name → uuid
// resolution: a non-UUID positional triggers a list-then-match before
// the by-id GET.
func TestNetworkGet_NameResolves(t *testing.T) {
	t.Parallel()
	id := uuid.NewString()
	var listed, fetched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/networks":
			listed = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[` + string(networkJSON(id, "net-named")) + `],"meta":{"next_cursor":null}}`))
		case "/v1/networks/" + id:
			fetched = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(networkJSON(id, "net-named"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	stdout, _, err := runNetworkCmd(t, srv.URL, []string{"get", "net-named"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !listed {
		t.Errorf("name resolution should have listed networks")
	}
	if !fetched {
		t.Errorf("by-id GET should have fired after resolution")
	}
	if !strings.Contains(stdout, "net-named") {
		t.Errorf("output missing network name:\n%s", stdout)
	}
}

func TestNetworkDelete_Happy(t *testing.T) {
	t.Parallel()
	id := uuid.NewString()
	var deletes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/v1/networks/"+id {
			t.Errorf("path = %s, want /v1/networks/%s", r.URL.Path, id)
		}
		deletes++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	stdout, _, err := runNetworkCmd(t, srv.URL, []string{"delete", id, "--force"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if deletes != 1 {
		t.Errorf("deletes = %d, want 1", deletes)
	}
	if !strings.Contains(stdout, "deleted") {
		t.Errorf("stdout missing deletion message: %q", stdout)
	}
}

func TestNetworkDelete_409Blocked(t *testing.T) {
	t.Parallel()
	id := uuid.NewString()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"resource_in_use","message":"network is in use by virtual machine NICs","details":{"blocking_resources":{"vm_nics":3}}}}`))
	}))
	defer srv.Close()

	stdout, _, err := runNetworkCmd(t, srv.URL, []string{"delete", id, "--force"})
	if err == nil {
		t.Fatalf("expected error for 409 blocked")
	}
	if !strings.Contains(stdout, "vm_nics: 3") {
		t.Errorf("stdout missing blocking vm_nics count:\n%s", stdout)
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("err = %v, want mention of blocked", err)
	}
}
