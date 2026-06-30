// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	agentheartbeat "github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// TestHeartbeatShipsSessionCAPublic pins the ingress-session CA distribution
// contract end to end: once the cluster session CA is provisioned, every
// heartbeat response carries its PEM-encoded public half, and the agent-side
// heartbeat Response struct decodes it into a key a gateway can parse. Without
// the agent-side mirror field the gateway never receives the CA public half and
// cannot verify session credentials, so the round-trip is the point of the field.
func TestHeartbeatShipsSessionCAPublic(t *testing.T) {
	h := newE2E(t)

	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)
	ag := wgSeedAgent(t, h, caCert, caKey, "node-sessionca")

	// Before the CA is provisioned the field is absent (fail-open, nil).
	if got := hbDecodeSessionCA(t, agentSrv.URL, ag); got != nil {
		t.Fatalf("session_ca_public_pem before provisioning = %q, want nil", *got)
	}

	// Provision the cluster ingress-session CA via the store.
	mat, err := auth.GenerateSessionCA()
	if err != nil {
		t.Fatalf("GenerateSessionCA: %v", err)
	}
	if _, err := h.store.CreateSessionCA(context.Background(), store.CreateSessionCAParams{
		PrivateKeyPEM: mat.PrivateKeyPEM, PublicKeyPEM: mat.PublicKeyPEM,
	}); err != nil {
		t.Fatalf("CreateSessionCA: %v", err)
	}

	got := hbDecodeSessionCA(t, agentSrv.URL, ag)
	if got == nil {
		t.Fatalf("session_ca_public_pem after provisioning = nil, want the CA public half")
	}
	if *got != string(mat.PublicKeyPEM) {
		t.Errorf("session_ca_public_pem = %q, want %q", *got, string(mat.PublicKeyPEM))
	}
	// The gateway must be able to parse what it receives.
	if _, err := auth.ParseSessionCAPublic([]byte(*got)); err != nil {
		t.Errorf("ParseSessionCAPublic on heartbeat-delivered PEM error = %v, want nil", err)
	}
}

// hbDecodeSessionCA posts a minimal heartbeat over mTLS for ag, asserts 200, and
// decodes the response through the agent-side heartbeat Response struct (the
// gateway's view), returning its SessionCAPublicPEM.
func hbDecodeSessionCA(t *testing.T, baseURL string, ag wgAgent) *string {
	t.Helper()
	body := map[string]any{
		"agent_version": "test-0.1.0",
		"architecture":  "amd64",
		"capabilities": map[string]any{
			"cpu_model":        "test-cpu",
			"cpu_flags":        []string{},
			"cpu_cores_total":  4,
			"memory_total_mib": 8192,
			"kernel_version":   "test",
			"qemu_version":     "test",
		},
		"resources": map[string]any{
			"cpu_cores_available":  4,
			"memory_available_mib": 8000,
		},
		"vms":      []any{},
		"networks": []any{},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal heartbeat: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		baseURL+"/v1/nodes/"+ag.name+"/heartbeat", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new heartbeat request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ag.client.Do(req)
	if err != nil {
		t.Fatalf("heartbeat Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("heartbeat status = %d, want 200; body=%s", resp.StatusCode, string(b))
	}
	var decoded agentheartbeat.Response
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode heartbeat response: %v", err)
	}
	return decoded.SessionCAPublicPEM
}
