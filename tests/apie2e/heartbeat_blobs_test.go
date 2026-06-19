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
)

// hbPostBlobs posts a heartbeat over mTLS for ag carrying a node-level blob
// inventory with the given blob set and blobs_unavailable flag, asserting 200.
// It drives the wire JSON directly (a raw map) so the test exercises the CP
// handler's tri-state decode without depending on the agent producer struct.
func hbPostBlobs(t *testing.T, baseURL string, ag wgAgent, blobs []map[string]any, unavailable bool) {
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
	if blobs != nil {
		body["blobs"] = blobs
	}
	if unavailable {
		body["blobs_unavailable"] = true
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

// TestHeartbeatBlobsUnavailablePreservesInventory pins the node-blob-inventory
// fail-closed contract end to end over the real CP heartbeat handler: a heartbeat reporting
// blobs_unavailable for a node PRESERVES the prior node_blobs inventory rather
// than clearing it (a transient agent enumerate-failure must not wipe the
// holder-discovery source), while a genuinely-enumerated empty list still clears
// it. Removing the skip-on-unavailable guard in applyBlobInventory must make the
// preservation assertion fail (the inventory would clear to zero).
func TestHeartbeatBlobsUnavailablePreservesInventory(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()

	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)
	ag := wgSeedAgent(t, h, caCert, caKey, "node-blobnode")

	digest := "00000000000000000000000000000000000000000000000000000000000000ab"

	// 1. Heartbeat with a populated blob -> inventory present.
	hbPostBlobs(t, agentSrv.URL, ag, []map[string]any{{
		"digest":     digest,
		"size_bytes": 1 << 20,
	}}, false)

	inv, err := h.store.NodeBlobInventory(ctx, ag.nodeID)
	if err != nil {
		t.Fatalf("NodeBlobInventory (after populate): %v", err)
	}
	if len(inv) != 1 || inv[0].Digest != digest {
		t.Fatalf("inventory after populate = %+v, want one blob %s", inv, digest)
	}

	// 2. Heartbeat with blobs_unavailable:true and no blobs -> inventory
	// PRESERVED (the transient-enumerate-error case).
	hbPostBlobs(t, agentSrv.URL, ag, nil, true)

	inv, err = h.store.NodeBlobInventory(ctx, ag.nodeID)
	if err != nil {
		t.Fatalf("NodeBlobInventory (after unavailable): %v", err)
	}
	if len(inv) != 1 || inv[0].Digest != digest {
		t.Fatalf("inventory after blobs_unavailable = %+v, want preserved (one blob %s)", inv, digest)
	}

	// 3. Heartbeat with blobs_unavailable:false and zero blobs -> CLEARED (the
	// genuinely-empty enumeration case).
	hbPostBlobs(t, agentSrv.URL, ag, []map[string]any{}, false)

	inv, err = h.store.NodeBlobInventory(ctx, ag.nodeID)
	if err != nil {
		t.Fatalf("NodeBlobInventory (after empty): %v", err)
	}
	if len(inv) != 0 {
		t.Fatalf("inventory after genuine-empty = %+v, want cleared (zero blobs)", inv)
	}
}
