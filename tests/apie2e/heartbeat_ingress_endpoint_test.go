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

	"github.com/otherix/otherix/internal/store"
)

// TestHeartbeatIngestsIngressAdvertisedEndpoint drives the self-report seam end to
// end: a co-located node posts a heartbeat carrying ingress_advertised_endpoint,
// the CP ingests it into the node row through the CAS-safe capability write, a
// later heartbeat WITHOUT the field preserves the stored value (preserve-on-empty
// keeps a good endpoint from being cleared by a transient tick), and the whole
// path never disturbs the operator-assigned gateway role.
func TestHeartbeatIngestsIngressAdvertisedEndpoint(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()

	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)
	ag := wgSeedAgent(t, h, caCert, caKey, "node-colocated")

	// The node is a gateway (role set by an operator toggle); the self-reported
	// endpoint must land as a capability without touching that role.
	if _, err := h.store.SetNodeGatewayRole(ctx, ag.nodeID, true); err != nil {
		t.Fatalf("SetNodeGatewayRole: %v", err)
	}

	const want = "https://node-colocated.example:9444"
	hbPostIngress(t, agentSrv.URL, ag, want)

	got, err := h.store.NodeByID(ctx, ag.nodeID)
	if err != nil {
		t.Fatalf("NodeByID after non-empty report: %v", err)
	}
	if got.IngressAdvertisedEndpoint != want {
		t.Errorf("IngressAdvertisedEndpoint = %q, want %q", got.IngressAdvertisedEndpoint, want)
	}
	if !got.HasRole(store.NodeRoleGateway) {
		t.Errorf("heartbeat ingest dropped the gateway role: HasRole(gateway) = false, want true")
	}

	// A heartbeat that omits the field must not clear the stored endpoint.
	hbPostIngress(t, agentSrv.URL, ag, "")

	got, err = h.store.NodeByID(ctx, ag.nodeID)
	if err != nil {
		t.Fatalf("NodeByID after empty report: %v", err)
	}
	if got.IngressAdvertisedEndpoint != want {
		t.Errorf("empty report cleared IngressAdvertisedEndpoint: got %q, want %q (preserved)", got.IngressAdvertisedEndpoint, want)
	}
	if !got.HasRole(store.NodeRoleGateway) {
		t.Errorf("empty heartbeat dropped the gateway role: HasRole(gateway) = false, want true")
	}
}

// hbPostIngress posts a minimal heartbeat over mTLS for ag, setting
// ingress_advertised_endpoint only when endpoint is non-empty (an empty endpoint
// omits the field, exercising the preserve-on-empty path), and asserts 200.
func hbPostIngress(t *testing.T, baseURL string, ag wgAgent, endpoint string) {
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
	if endpoint != "" {
		body["ingress_advertised_endpoint"] = endpoint
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
}
