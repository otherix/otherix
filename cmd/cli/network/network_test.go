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
		if body["name"] != "net-dev" {
			t.Errorf("name = %v, want net-dev", body["name"])
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
		_, _ = w.Write(networkJSON(uuid.NewString(), "net-dev"))
	}))
	defer srv.Close()

	stdout, _, err := runNetworkCmd(t, srv.URL, []string{
		"create", "net-dev", "--bridge-name", "br0",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1", posts)
	}
	if !strings.Contains(stdout, "network net-dev created") {
		t.Errorf("stdout missing creation message: %q", stdout)
	}
}

func TestNetworkCreate_MissingBridgeName(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when required flag is missing")
	}))
	defer srv.Close()

	_, _, err := runNetworkCmd(t, srv.URL, []string{"create", "net-dev"})
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

func TestNetworkCreate_OverlayEgressBody(t *testing.T) {
	t.Parallel()
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["type"] != "overlay" {
			t.Errorf("type = %v, want overlay", body["type"])
		}
		if body["egress"] != "nat" {
			t.Errorf("egress = %v, want nat", body["egress"])
		}
		if body["subnet"] != "10.50.0.0/24" {
			t.Errorf("subnet = %v, want 10.50.0.0/24", body["subnet"])
		}
		if _, present := body["bridge_name"]; present {
			t.Errorf("bridge_name must be absent for overlay, got %v", body["bridge_name"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(networkJSON(uuid.NewString(), "net-overlay"))
	}))
	defer srv.Close()

	_, _, err := runNetworkCmd(t, srv.URL, []string{
		"create", "net-overlay", "--type", "overlay",
		"--subnet", "10.50.0.0/24", "--egress", "nat",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1", posts)
	}
}

func TestNetworkCreate_DhcpBody(t *testing.T) {
	t.Parallel()
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["dhcp"] != true {
			t.Errorf("dhcp = %v, want true", body["dhcp"])
		}
		if body["type"] != "overlay" {
			t.Errorf("type = %v, want overlay", body["type"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(networkJSON(uuid.NewString(), "net-dhcp"))
	}))
	defer srv.Close()

	_, _, err := runNetworkCmd(t, srv.URL, []string{
		"create", "net-dhcp", "--type", "overlay",
		"--subnet", "10.50.0.0/24", "--egress", "nat", "--dhcp",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1", posts)
	}
}

func TestNetworkCreate_OverlayDNSBody(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantKey bool
		wantVal bool
	}{
		// No --dns: omit the key so the server defaults dns to the dhcp value.
		{"omitted", []string{"create", "ovl", "--type", "overlay", "--subnet", "10.50.0.0/24"}, false, false},
		// --dns=false: send it verbatim (must reach the server, not be defaulted).
		{"explicit-false", []string{"create", "ovl", "--type", "overlay", "--subnet", "10.50.0.0/24", "--dns=false"}, true, false},
		{"explicit-true", []string{"create", "ovl", "--type", "overlay", "--subnet", "10.50.0.0/24", "--dns"}, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				v, present := body["dns"]
				if present != tc.wantKey {
					t.Errorf("dns key present = %v, want %v (body=%v)", present, tc.wantKey, body)
				}
				if tc.wantKey && v != tc.wantVal {
					t.Errorf("dns = %v, want %v", v, tc.wantVal)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write(networkJSON(uuid.NewString(), "ovl"))
			}))
			defer srv.Close()
			if _, _, err := runNetworkCmd(t, srv.URL, tc.args); err != nil {
				t.Fatalf("Execute: %v", err)
			}
		})
	}
}

func TestNetworkCreate_DhcpOmittedWhenAbsent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, present := body["dhcp"]; present {
			t.Errorf("dhcp should be omitted when --dhcp absent, got %v", body["dhcp"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(networkJSON(uuid.NewString(), "net-dev"))
	}))
	defer srv.Close()

	_, _, err := runNetworkCmd(t, srv.URL, []string{
		"create", "net-dev", "--bridge-name", "br0",
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
		"create", "net-dev", "--bridge-name", "br0",
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

func TestNetworkCreate_OverlayMissingSubnet(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when validation fails client-side")
	}))
	defer srv.Close()

	_, _, err := runNetworkCmd(t, srv.URL, []string{
		"create", "net-overlay", "--type", "overlay",
	})
	if err == nil {
		t.Fatalf("expected error for missing --subnet")
	}
	if !strings.Contains(err.Error(), "--subnet") {
		t.Errorf("err = %v, want mention of --subnet", err)
	}
}

func TestNetworkCreate_OverlayForbidsBridgeName(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("HTTP call must not happen when validation fails client-side")
	}))
	defer srv.Close()

	_, _, err := runNetworkCmd(t, srv.URL, []string{
		"create", "net-overlay", "--type", "overlay",
		"--subnet", "10.50.0.0/24",
		"--bridge-name", "br0",
	})
	if err == nil {
		t.Fatalf("expected error for --bridge-name with --type overlay")
	}
	if !strings.Contains(err.Error(), "--bridge-name") {
		t.Errorf("err = %v, want mention of --bridge-name", err)
	}
}

func TestNetworkCreate_OverlayHappy(t *testing.T) {
	t.Parallel()
	vni := 10050
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["type"] != "overlay" {
			t.Errorf("type = %v, want overlay", body["type"])
		}
		if body["subnet"] != "10.50.0.0/24" {
			t.Errorf("subnet = %v, want 10.50.0.0/24", body["subnet"])
		}
		if _, present := body["bridge_name"]; present {
			t.Errorf("bridge_name must be absent for overlay, got %v", body["bridge_name"])
		}
		resp := map[string]any{
			"id":          uuid.NewString(),
			"name":        "net-overlay",
			"type":        "overlay",
			"bridge_name": "",
			"managed":     false,
			"egress":      "none",
			"vlan_tag":    nil,
			"mtu":         1500,
			"subnet":      "10.50.0.0/24",
			"gateway":     nil,
			"vni":         vni,
			"config":      map[string]any{},
			"created_at":  "2026-06-01T10:00:00Z",
			"updated_at":  "2026-06-01T10:00:00Z",
		}
		raw, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	stdout, _, err := runNetworkCmd(t, srv.URL, []string{
		"create", "net-overlay", "--type", "overlay", "--subnet", "10.50.0.0/24",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if posts != 1 {
		t.Errorf("posts = %d, want 1", posts)
	}
	if !strings.Contains(stdout, "net-overlay") {
		t.Errorf("stdout missing network name:\n%s", stdout)
	}
	if !strings.Contains(stdout, "10050") {
		t.Errorf("stdout missing vni value:\n%s", stdout)
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
	for _, want := range []string{"NAME", "TYPE", "BRIDGE", "MANAGED", "EGRESS", "CIDR", "MTU", "net-a", "net-b"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table missing %q:\n%s", want, stdout)
		}
	}
}

// TestNetworkList_CIDRColumn checks the CIDR column renders a network's subnet
// and falls back to "-" when the network has none.
func TestNetworkList_CIDRColumn(t *testing.T) {
	t.Parallel()
	withSubnet := map[string]any{
		"id": uuid.NewString(), "name": "ov-net", "type": "overlay", "bridge_name": "otb1000",
		"managed": true, "egress": "nat", "vlan_tag": nil, "mtu": 1390,
		"subnet": "10.62.0.0/24", "gateway": nil, "config": map[string]any{},
		"created_at": "2026-06-01T10:00:00Z", "updated_at": "2026-06-01T10:00:00Z",
	}
	withSubnetJSON, _ := json.Marshal(withSubnet)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[` + string(withSubnetJSON) + `,` +
			string(networkJSON(uuid.NewString(), "bridge-net")) + `],"meta":{"next_cursor":null}}`))
	}))
	defer srv.Close()

	stdout, _, err := runNetworkCmd(t, srv.URL, []string{"list"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(stdout, "10.62.0.0/24") {
		t.Errorf("table missing CIDR value 10.62.0.0/24:\n%s", stdout)
	}
	// bridge-net has subnet=nil -> "-" placeholder.
	if !strings.Contains(stdout, "-") {
		t.Errorf("table missing %q placeholder for a subnet-less network:\n%s", "-", stdout)
	}
}

// TestNetworkCommand_NetAlias guards the `net` shorthand for `network`.
func TestNetworkCommand_NetAlias(t *testing.T) {
	t.Parallel()
	aliases := network.NewCommand().Aliases
	found := false
	for _, a := range aliases {
		if a == "net" {
			found = true
		}
	}
	if !found {
		t.Errorf("network command aliases = %v, want to include %q", aliases, "net")
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
			"id": id, "name": "net-dev", "type": "bridge", "bridge_name": "br0",
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
			"id": id, "name": "net-dev", "type": "bridge", "bridge_name": "br0",
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

// TestNetworkGet_NameResolvesCaseInsensitively confirms the client-side
// name resolution matches case-insensitively, mirroring the store's
// lowercased name guard and the server-side NetworkByName lookup. An
// operator passing NET-NAMED must resolve a network stored as net-named.
func TestNetworkGet_NameResolvesCaseInsensitively(t *testing.T) {
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

	stdout, _, err := runNetworkCmd(t, srv.URL, []string{"get", "NET-NAMED"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !listed {
		t.Errorf("name resolution should have listed networks")
	}
	if !fetched {
		t.Errorf("by-id GET should have fired after case-insensitive resolution")
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

// TestNetworkGet_OverlayRendersVNI confirms that renderGet emits a vni line
// for an overlay network and does not emit a blank bridge_name line when
// bridge_name is empty.
func TestNetworkGet_OverlayRendersVNI(t *testing.T) {
	t.Parallel()
	id := uuid.NewString()
	vni := 10050
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/networks/"+id {
			t.Errorf("path = %s, want /v1/networks/%s", r.URL.Path, id)
		}
		body := map[string]any{
			"id":          id,
			"name":        "net-overlay",
			"type":        "overlay",
			"bridge_name": "",
			"managed":     false,
			"egress":      "none",
			"vlan_tag":    nil,
			"mtu":         1500,
			"subnet":      "10.50.0.0/24",
			"gateway":     nil,
			"vni":         vni,
			"config":      map[string]any{},
			"created_at":  "2026-06-01T10:00:00Z",
			"updated_at":  "2026-06-01T10:00:00Z",
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
	if !strings.Contains(stdout, "vni: 10050") {
		t.Errorf("stdout missing vni line:\n%s", stdout)
	}
	// bridge_name must not appear at all when it is empty - no blank line.
	if strings.Contains(stdout, "bridge_name:") {
		t.Errorf("stdout must not contain bridge_name line for overlay network:\n%s", stdout)
	}
}
