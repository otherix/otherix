// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cluster_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClusterSetSSHIngress confirms `cluster set-ssh-ingress --enabled
// --suffix` issues a PUT /v1/cluster/ssh-ingress carrying the master switch
// and suffix, and renders the server's echo.
func TestClusterSetSSHIngress(t *testing.T) {
	t.Parallel()
	var puts int
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		puts++
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"enabled":true,"cluster_suffix":"ssh.otherix.local"}`))
	}))
	defer srv.Close()

	stdout, _, err := runClusterCmd(t, srv.URL, []string{
		"set-ssh-ingress", "--enabled", "--suffix", "ssh.otherix.local",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if puts != 1 {
		t.Errorf("puts = %d, want 1", puts)
	}
	if gotMethod != http.MethodPut || gotPath != "/v1/cluster/ssh-ingress" {
		t.Errorf("request = %s %s, want PUT /v1/cluster/ssh-ingress", gotMethod, gotPath)
	}
	if enabled, _ := gotBody["enabled"].(bool); !enabled {
		t.Errorf("body enabled = %v, want true", gotBody["enabled"])
	}
	if gotBody["cluster_suffix"] != "ssh.otherix.local" {
		t.Errorf("body cluster_suffix = %v, want ssh.otherix.local", gotBody["cluster_suffix"])
	}
	if !strings.Contains(stdout, "ssh.otherix.local") {
		t.Errorf("stdout missing suffix:\n%s", stdout)
	}
}

// TestClusterSetSSHIngressDisable confirms --enabled=false sends
// enabled:false (the disable path).
func TestClusterSetSSHIngressDisable(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"enabled":false,"cluster_suffix":""}`))
	}))
	defer srv.Close()

	_, _, err := runClusterCmd(t, srv.URL, []string{"set-ssh-ingress", "--enabled=false"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if enabled, ok := gotBody["enabled"].(bool); !ok || enabled {
		t.Errorf("body enabled = %v, want false", gotBody["enabled"])
	}
}

// TestClusterGetSSHIngress confirms `cluster get-ssh-ingress` issues a GET
// and renders the master switch and suffix.
func TestClusterGetSSHIngress(t *testing.T) {
	t.Parallel()
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"enabled":true,"cluster_suffix":"ssh.otherix.local"}`))
	}))
	defer srv.Close()

	stdout, _, err := runClusterCmd(t, srv.URL, []string{"get-ssh-ingress"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/v1/cluster/ssh-ingress" {
		t.Errorf("request = %s %s, want GET /v1/cluster/ssh-ingress", gotMethod, gotPath)
	}
	if !strings.Contains(stdout, "enabled=true") || !strings.Contains(stdout, "ssh.otherix.local") {
		t.Errorf("stdout missing rendered fields:\n%s", stdout)
	}
}
