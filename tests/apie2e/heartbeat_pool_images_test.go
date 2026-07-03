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

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// hbPostPoolImages posts a heartbeat over mTLS for ag carrying a single pool
// report with the given image set and images_unavailable flag, asserting 200.
// It drives the wire JSON directly (a raw map) so the test exercises the CP
// handler's tri-state decode without depending on the agent producer struct.
func hbPostPoolImages(t *testing.T, baseURL string, ag wgAgent, poolName string, images []map[string]any, unavailable bool) {
	t.Helper()
	pool := map[string]any{
		"name":                  poolName,
		"reconciliation_status": "ready",
	}
	if images != nil {
		pool["images"] = images
	}
	if unavailable {
		pool["images_unavailable"] = true
	}
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
		"pools":    []any{pool},
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

// TestHeartbeatImagesUnavailablePreservesInventory pins the fail-closed
// contract end to end over the real CP heartbeat handler: a heartbeat reporting
// images_unavailable for a pool PRESERVES the prior inventory rather than
// clearing it, while a genuinely-enumerated empty list still clears it.
func TestHeartbeatImagesUnavailablePreservesInventory(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()

	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)
	ag := wgSeedAgent(t, h, caCert, caKey, "node-imgpool")

	// Seed a storage pool on the agent's node so the inventory write resolves.
	poolID := uuid.New()
	poolName := "pool-imgpool"
	if _, err := h.store.CreateStoragePool(ctx, store.CreateStoragePoolParams{
		ID: poolID, NodeID: ag.nodeID, Name: poolName, Type: "local_dir",
		Path: "/var/lib/otherix/pools/" + poolName, Config: []byte(`{}`),
	}); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}

	// 1. Heartbeat with a populated image -> inventory present.
	hbPostPoolImages(t, agentSrv.URL, ag, poolName, []map[string]any{{
		"basename":           "noble.qcow2",
		"sha256":             "abc123",
		"size_bytes":         1 << 20,
		"virtual_size_bytes": 2 << 30,
		"format":             "qcow2",
		"imported_at":        "2026-06-13T00:00:00Z",
	}}, false)

	inv, err := h.store.PoolImageInventory(ctx, poolID)
	if err != nil {
		t.Fatalf("PoolImageInventory (after populate): %v", err)
	}
	if len(inv) != 1 || inv[0].Basename != "noble.qcow2" {
		t.Fatalf("inventory after populate = %+v, want one image noble.qcow2", inv)
	}

	// 2. Heartbeat for the same pool with images_unavailable:true and no images
	// -> inventory PRESERVED (the transient-enumerate-error case).
	hbPostPoolImages(t, agentSrv.URL, ag, poolName, nil, true)

	inv, err = h.store.PoolImageInventory(ctx, poolID)
	if err != nil {
		t.Fatalf("PoolImageInventory (after unavailable): %v", err)
	}
	if len(inv) != 1 || inv[0].Basename != "noble.qcow2" {
		t.Fatalf("inventory after images_unavailable = %+v, want preserved (one image noble.qcow2)", inv)
	}

	// 3. Heartbeat with images_unavailable:false and zero images -> CLEARED (the
	// genuinely-empty enumeration case).
	hbPostPoolImages(t, agentSrv.URL, ag, poolName, []map[string]any{}, false)

	inv, err = h.store.PoolImageInventory(ctx, poolID)
	if err != nil {
		t.Fatalf("PoolImageInventory (after empty): %v", err)
	}
	if len(inv) != 0 {
		t.Fatalf("inventory after genuine-empty = %+v, want cleared (zero images)", inv)
	}
}

// TestHeartbeatSnapshotsFieldAccepted pins the agent->CP heartbeat-acceptance
// seam for the snapshots[] inventory field (VM snapshot inventory). The CP
// heartbeat decoder uses DisallowUnknownFields(), so an upgraded agent that
// reports a pool's snapshot-blob cache via pools[].snapshots[] (plus
// snapshots_unavailable) would 400 validation_failed unless the CP receiver
// struct declares the matching fields. This drives the REAL HTTP decode path
// (raw wire map -> json.Decoder.DisallowUnknownFields) and asserts the body is
// accepted (2xx), not rejected. Removing Snapshots/SnapshotsUnavailable from the
// CP receiver struct must make this test 400.
func TestHeartbeatSnapshotsFieldAccepted(t *testing.T) {
	h := newE2E(t)

	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)
	ag := wgSeedAgent(t, h, caCert, caKey, "node-snappool")

	// A pool entry carrying snapshots[] + snapshots_unavailable:false. No pool
	// row is seeded: the snapshot inventory is not yet projected CP-side (the
	// agent only reports it), so acceptance must hold on the decode seam alone.
	pool := map[string]any{
		"name":                  "pool-snappool",
		"reconciliation_status": "ready",
		"snapshots": []map[string]any{{
			"sha256":     "0000000000000000000000000000000000000000000000000000000000000000",
			"size_bytes": 123,
		}},
		"snapshots_unavailable": false,
	}
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
		"pools":    []any{pool},
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("heartbeat with snapshots[] status = %d, want 2xx (DisallowUnknownFields must accept the field); body=%s",
			resp.StatusCode, string(b))
	}
}
