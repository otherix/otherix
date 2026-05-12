// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otherix/otherix/internal/api/agentclient"
)

// healthBody is the JSON shape an Iteration 1+ agent emits at /health.
// Re-declared as a literal here so test failures show the shape on
// the wire without dragging in the agent-side type.
func healthBody() map[string]any {
	return map[string]any{
		"status":  "ok",
		"node_id": "node-test",
		"build":   "abc1234",
		"version": "iteration-1",
		"time":    "2026-05-09T20:00:00Z",
	}
}

func TestHealth_HappyPath(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		jsonWrite(t, w, http.StatusOK, healthBody())
	})

	_, baseURL := startAgentTLSServer(t, mux)

	client := newClient(t, dir)

	resp, err := client.Health(context.Background(), baseURL)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}

	if resp.Status != "ok" {
		t.Errorf("Status = %q, want %q", resp.Status, "ok")
	}
	if resp.NodeID != "node-test" {
		t.Errorf("NodeID = %q, want %q", resp.NodeID, "node-test")
	}
	if resp.Build != "abc1234" {
		t.Errorf("Build = %q, want %q", resp.Build, "abc1234")
	}
	if resp.Version != "iteration-1" {
		t.Errorf("Version = %q, want %q", resp.Version, "iteration-1")
	}
	if resp.Time.IsZero() {
		t.Errorf("Time was not parsed; got zero value")
	}
}

func TestHealth_AgentError5xx(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		jsonWrite(t, w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{
				"code":    "internal",
				"message": "agent not ready",
			},
		})
	})

	_, baseURL := startAgentTLSServer(t, mux)

	client := newClient(t, dir)

	_, err := client.Health(context.Background(), baseURL)
	if err == nil {
		t.Fatalf("Health on 503 returned nil error")
	}

	var agentErr *agentclient.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("err = %v; want *AgentError", err)
	}
	if agentErr.Status != http.StatusServiceUnavailable {
		t.Errorf("Status = %d, want 503", agentErr.Status)
	}
	if !agentErr.IsRetryable() {
		t.Errorf("IsRetryable() = false, want true for 5xx")
	}
}

func TestHealth_MalformedJSON(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	})

	_, baseURL := startAgentTLSServer(t, mux)

	client := newClient(t, dir)

	_, err := client.Health(context.Background(), baseURL)
	if err == nil {
		t.Fatalf("Health on malformed body returned nil error")
	}
	if !strings.Contains(err.Error(), "decode health response") {
		t.Errorf("err = %v; want substring %q", err, "decode health response")
	}
}

func TestHealth_ConnectionRefused(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)

	// Start a TLS server, capture its URL, then close it before the
	// client dials. The client now hits a closed listener — production
	// equivalent of agent process being down.
	mux := http.NewServeMux()
	srv, baseURL := startAgentTLSServer(t, mux)
	srv.Close()

	client := newClient(t, dir)

	_, err := client.Health(context.Background(), baseURL)
	if err == nil {
		t.Fatalf("Health against closed listener returned nil error")
	}
	// Don't pin к exact error string — Go versions vary on
	// "connection refused" vs "connect: connection refused" — but
	// we expect SOMETHING on the get-/health wrapper.
	if !strings.Contains(err.Error(), "agentclient: get /health") {
		t.Errorf("err = %v; want substring %q", err, "agentclient: get /health")
	}
}

func TestLoadMaterialFromFiles_BadCAFile(t *testing.T) {
	t.Parallel()
	dir := writeMaterial(t)

	// Overwrite ca.crt with junk — LoadMaterialFromFiles must reject.
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("definitely not PEM"), 0o600); err != nil {
		t.Fatalf("clobber ca.crt: %v", err)
	}

	_, _, err := agentclient.LoadMaterialFromFiles(
		filepath.Join(dir, "tls.crt"),
		filepath.Join(dir, "tls.key"),
		filepath.Join(dir, "ca.crt"),
	)
	if err == nil {
		t.Fatalf("LoadMaterialFromFiles(bad ca) returned nil error")
	}
	if !strings.Contains(err.Error(), "CERTIFICATE PEM block") {
		t.Errorf("err = %v; want substring %q", err, "CERTIFICATE PEM block")
	}
}
