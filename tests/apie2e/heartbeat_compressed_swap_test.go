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

	"github.com/otherix/otherix/internal/auth"
)

// TestHeartbeatCompressedSwapReachesNodeView pins the compressed-swap CP seam: an agent
// heartbeat carrying capabilities.compressed_swap must survive the strict
// (DisallowUnknownFields) decode AND the buildCapabilitiesJSON projection so it
// reaches the node view's raw capabilities blob. This drives the REAL HTTP
// heartbeat handler (mTLS), then GETs the node as admin. Reverting either the
// decode field or the projection must fail this test: without the decode field
// the heartbeat 400s; without the projection compressed_swap is dropped.
func TestHeartbeatCompressedSwapReachesNodeView(t *testing.T) {
	h := newE2E(t)

	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)
	ag := wgSeedAgent(t, h, caCert, caKey, "node-cswap")

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
			"compressed_swap": map[string]any{
				"kind":          "zram",
				"size_mib":      768,
				"mem_limit_mib": 256,
				"algorithm":     "zstd",
			},
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
		agentSrv.URL+"/v1/nodes/"+ag.name+"/heartbeat", bytes.NewReader(raw))
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
		t.Fatalf("heartbeat status = %d, want 200 (decode must accept compressed_swap); body=%s",
			resp.StatusCode, string(b))
	}

	// GET the node as admin and confirm compressed_swap survived to the raw
	// capabilities blob.
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	getResp := h.get(t, "/v1/nodes/"+ag.name, admin)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get node status = %d, want 200", getResp.StatusCode)
	}
	var node struct {
		Capabilities json.RawMessage `json:"capabilities"`
	}
	decodeJSON(t, getResp, &node)

	var caps struct {
		CompressedSwap *struct {
			Algorithm   string `json:"algorithm"`
			MemLimitMib int64  `json:"mem_limit_mib"`
		} `json:"compressed_swap"`
	}
	if err := json.Unmarshal(node.Capabilities, &caps); err != nil {
		t.Fatalf("unmarshal node capabilities: %v", err)
	}
	if caps.CompressedSwap == nil || caps.CompressedSwap.Algorithm != "zstd" || caps.CompressedSwap.MemLimitMib != 256 {
		t.Fatalf("compressed_swap dropped at the CP boundary: %s", node.Capabilities)
	}
}
