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

// TestHeartbeatGoneNodeSelfHeals is the seam test for retiring the timer-driven
// gone state: a node whose stored row is still gone (the transition window) must
// be allowed to heartbeat rather than rejected with 409 node_gone. The heartbeat
// handler stamps its liveness (last_heartbeat_at), which is precisely what lets
// the reconcile rewrite the row to unreachable and PromoteHealthyNodes then
// advance it to ready without an operator readmit. With the 409 fence still in
// place this test fails: the projection returns 409 node_gone and never reaches
// applyNodeUpdate, so last_heartbeat_at stays nil.
func TestHeartbeatGoneNodeSelfHeals(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()

	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)
	ag := wgSeedAgentWithStatus(t, h, caCert, caKey, "node-gone-heal", store.NodeStatus("gone"))

	// A gone node has not heartbeat yet in this test, so liveness starts nil.
	before, err := h.store.NodeByID(ctx, ag.nodeID)
	if err != nil {
		t.Fatalf("NodeByID (before): %v", err)
	}
	if before.LastHeartbeatAt != nil {
		t.Fatalf("seeded gone node already has last_heartbeat_at=%v, want nil", before.LastHeartbeatAt)
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
		"pools":    []any{},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal heartbeat: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
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
		t.Fatalf("heartbeat from gone node status = %d, want 200 (the 409 node_gone fence must be gone); body=%s",
			resp.StatusCode, string(b))
	}

	// applyNodeUpdate runs LAST in the projection; a 200 means it ran and stamped
	// liveness, which is the signal the reconcile/promotion path keys on.
	after, err := h.store.NodeByID(ctx, ag.nodeID)
	if err != nil {
		t.Fatalf("NodeByID (after): %v", err)
	}
	if after.LastHeartbeatAt == nil {
		t.Errorf("last_heartbeat_at is nil after heartbeat; a gone node's heartbeat must stamp liveness")
	}
}
