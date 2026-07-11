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

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/store"
)

// TestNodeGetMemoryOvercommitView pins the memory-overcommit observability seam:
// with overcommit enabled (ratio 2.0) and an agent reporting a qualifying zram
// compressed-swap device, the effective node get view must carry a
// memory_overcommit block with eligible=true, the zram-bounded headroom, and the
// aggregate balloon real-used across the node's running VMs. It drives the REAL
// mTLS heartbeat handler for the capabilities, seeds running VM runtime rows for
// the aggregate, then GETs the node as admin.
func TestNodeGetMemoryOvercommitView(t *testing.T) {
	h := newE2E(t, withPlacementResources(config.ResourcesConfig{
		Memory: config.MemoryResourceConfig{
			Enabled:                  true,
			OvercommitRatio:          2.0,
			OvercommitZramFloorMib:   256,
			OvercommitZramConfidence: 1.0,
		},
	}))

	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)
	ag := wgSeedAgent(t, h, caCert, caKey, "node-overcommit")

	// Heartbeat carries the zram device + memory_total the headroom model reads.
	// size_mib=2048, confidence 1.0 -> zram ceil 2048; operator ceil
	// 8192*(2.0-1.0)=8192; headroom = min = 2048.
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
				"kind":      "zram",
				"size_mib":  2048,
				"algorithm": "zstd",
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
		t.Fatalf("heartbeat status = %d, want 200; body=%s", resp.StatusCode, string(b))
	}

	// Seed two running VM runtime rows on the node so the aggregate real-used is
	// 1000+500=1500. A third with a nil reading must not shift the sum.
	seedNodeRuntime(t, h, ag.nodeID, store.VmPhaseRunning, int64Ptr(1000))
	seedNodeRuntime(t, h, ag.nodeID, store.VmPhaseRunning, int64Ptr(500))
	seedNodeRuntime(t, h, ag.nodeID, store.VmPhaseRunning, nil)

	admin, _ := loginAs(t, h, auth.RoleAdmin)
	getResp := h.get(t, "/v1/nodes/"+ag.name, admin)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get node status = %d, want 200", getResp.StatusCode)
	}
	var node struct {
		MemoryOvercommit *struct {
			Eligible    bool   `json:"eligible"`
			HeadroomMiB int64  `json:"headroom_mib"`
			RealUsedMiB *int64 `json:"real_used_mib"`
		} `json:"memory_overcommit"`
	}
	decodeJSON(t, getResp, &node)

	if node.MemoryOvercommit == nil {
		t.Fatalf("memory_overcommit absent from node get view")
	}
	mo := node.MemoryOvercommit
	if !mo.Eligible || mo.HeadroomMiB != 2048 {
		t.Errorf("memory_overcommit = %+v, want eligible headroom_mib=2048", mo)
	}
	if mo.RealUsedMiB == nil || *mo.RealUsedMiB != 1500 {
		t.Errorf("real_used_mib = %v, want 1500", mo.RealUsedMiB)
	}
}

func seedNodeRuntime(t *testing.T, h *harness, nodeID uuid.UUID, phase store.VMPhase, reading *int64) {
	t.Helper()
	ctx := context.Background()
	if err := h.store.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		return hp.UpsertVMRuntime(ctx, store.UpsertVMRuntimeParams{
			VmID: uuid.New(), CurrentNodeID: &nodeID, Phase: phase, MemoryUsedMib: reading,
		})
	}); err != nil {
		t.Fatalf("seedNodeRuntime: %v", err)
	}
}

func int64Ptr(v int64) *int64 { return &v }
