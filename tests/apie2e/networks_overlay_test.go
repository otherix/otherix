// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
)

// overlayNetworkView is the minimal decode shape for overlay-specific fields
// returned by POST /v1/networks.
type overlayNetworkView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	BridgeName string `json:"bridge_name"`
	Managed    bool   `json:"managed"`
	MTU        int32  `json:"mtu"`
	VNI        *int32 `json:"vni"`
}

// heartbeatDeclaredNetworkEntry is the minimal shape for one entry in the
// declared_networks slice of a heartbeat response.
type heartbeatDeclaredNetworkEntry struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// heartbeatDeclaredNetworksResponse decodes only declared_networks from the
// heartbeat response body.
type heartbeatDeclaredNetworksResponse struct {
	DeclaredNetworks []heartbeatDeclaredNetworkEntry `json:"declared_networks"`
}

func TestOverlayNetworkCreateAllocatesVNI(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	body := map[string]any{
		"name":   "ov-mvp-" + uuid.NewString()[:8],
		"type":   "overlay",
		"subnet": "10.50.0.0/24",
	}
	resp := h.post(t, "/v1/networks", body, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create overlay status = %d, want 201", resp.StatusCode)
	}
	var created overlayNetworkView
	decodeJSON(t, resp, &created)

	if created.Type != "overlay" {
		t.Errorf("type = %q, want overlay", created.Type)
	}
	if created.VNI == nil {
		t.Fatalf("vni is nil, want a non-nil allocated VNI")
	}
	wantBridgeName := fmt.Sprintf("otb%d", *created.VNI)
	if created.BridgeName != wantBridgeName {
		t.Errorf("bridge_name = %q, want %q (otb<vni>)", created.BridgeName, wantBridgeName)
	}
	if !created.Managed {
		t.Errorf("managed = false, want true")
	}
	if created.MTU != 1390 {
		t.Errorf("mtu = %d, want 1390", created.MTU)
	}
}

func TestOverlayNetworkCreateRejectsBridgeName(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	body := map[string]any{
		"name":        "ov-bad-" + uuid.NewString()[:8],
		"type":        "overlay",
		"subnet":      "10.51.0.0/24",
		"bridge_name": "otb9",
	}
	resp := h.post(t, "/v1/networks", body, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create overlay with bridge_name status = %d, want 400", resp.StatusCode)
	}
	assertErrorCode(t, resp, "validation_failed")
}

func TestOverlayNetworkCreateRequiresSubnet(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	body := map[string]any{
		"name": "ov-nosub-" + uuid.NewString()[:8],
		"type": "overlay",
	}
	resp := h.post(t, "/v1/networks", body, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create overlay without subnet status = %d, want 400", resp.StatusCode)
	}
	assertErrorCode(t, resp, "validation_failed")
}

// TestOverlayNetworkNotDeclaredToAgents verifies that overlay networks are
// filtered out of the declared_networks down-channel. It seeds one bridge and
// one overlay network, drives a synthetic agent heartbeat over the mTLS agent
// router (same pattern as wireguard_test.go), and asserts that no entry with
// type="overlay" appears in declared_networks.
func TestOverlayNetworkNotDeclaredToAgents(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	suffix := uuid.NewString()[:8]

	// Create a bridge network (capture its ID for the positive assertion below).
	bridgeResp := h.post(t, "/v1/networks", map[string]any{
		"name":        "br-" + suffix,
		"type":        "bridge",
		"bridge_name": "br" + suffix[:6],
	}, admin)
	if bridgeResp.StatusCode != http.StatusCreated {
		t.Fatalf("create bridge status = %d, want 201", bridgeResp.StatusCode)
	}
	var bridgeCreated struct {
		ID string `json:"id"`
	}
	decodeJSON(t, bridgeResp, &bridgeCreated)
	bridgeID := bridgeCreated.ID

	// Create an overlay network.
	overlayResp := h.post(t, "/v1/networks", map[string]any{
		"name":   "ov-" + suffix,
		"type":   "overlay",
		"subnet": "10.52.0.0/24",
	}, admin)
	if overlayResp.StatusCode != http.StatusCreated {
		t.Fatalf("create overlay status = %d, want 201", overlayResp.StatusCode)
	}
	overlayResp.Body.Close()

	// Seed a synthetic agent and drive a heartbeat over the agent mTLS router.
	caCert, caKey := wgGenerateCA(t)
	agentSrv := wgStartAgentTLSServer(t, h, caCert, caKey)
	ag := wgSeedAgent(t, h, caCert, caKey, "node-ovtest")

	hbBody := wgHeartbeatRequest{
		AgentVersion: "test-0.1.0",
		Architecture: "amd64",
		Capabilities: wgHeartbeatCaps{
			CPUModel:       "test-cpu",
			CPUFlags:       []string{},
			CPUCoresTotal:  4,
			MemoryTotalMib: 8192,
			KernelVersion:  "test",
			QEMUVersion:    "test",
			Firmwares:      []struct{}{},
		},
		Resources: wgHeartbeatRes{
			CPUCoresAvailable:  4,
			MemoryAvailableMib: 8000,
		},
		VMs:      []struct{}{},
		Networks: []struct{}{},
	}
	raw, err := json.Marshal(hbBody)
	if err != nil {
		t.Fatalf("marshal heartbeat: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		agentSrv.URL+"/v1/nodes/"+ag.name+"/heartbeat", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new heartbeat request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	hbResp, err := ag.client.Do(req)
	if err != nil {
		t.Fatalf("heartbeat Do: %v", err)
	}
	defer hbResp.Body.Close()
	if hbResp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat status = %d, want 200", hbResp.StatusCode)
	}
	var decoded heartbeatDeclaredNetworksResponse
	if err := json.NewDecoder(hbResp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode heartbeat response: %v", err)
	}

	bridgeSeen := false
	for _, n := range decoded.DeclaredNetworks {
		if n.Type == "overlay" {
			t.Errorf("declared_networks contains overlay entry id=%q; overlay must be filtered out until N3b", n.ID)
		}
		if n.ID == bridgeID {
			bridgeSeen = true
		}
	}
	if !bridgeSeen {
		t.Errorf("declared_networks does not contain the seeded bridge network id=%q; bridge networks must be declared to agents", bridgeID)
	}
}
