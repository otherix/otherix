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
func TestOverlayNetworkPatchOnlyName(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	// Create an overlay network.
	createBody := map[string]any{
		"name":   "ov-patch-" + uuid.NewString()[:8],
		"type":   "overlay",
		"subnet": "10.53.0.0/24",
	}
	createResp := h.post(t, "/v1/networks", createBody, admin)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create overlay status = %d, want 201", createResp.StatusCode)
	}
	var created overlayNetworkView
	decodeJSON(t, createResp, &created)

	// PATCH with only name -> expect 200.
	patchResp := h.patch(t, "/v1/networks/"+created.ID, map[string]any{"name": "ov-renamed"}, admin)
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("patch name status = %d, want 200", patchResp.StatusCode)
	}
	patchResp.Body.Close()

	// PATCH with subnet -> expect 400 validation_failed.
	subnetResp := h.patch(t, "/v1/networks/"+created.ID, map[string]any{"subnet": "10.60.0.0/24"}, admin)
	if subnetResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("patch subnet on overlay status = %d, want 400", subnetResp.StatusCode)
	}
	assertErrorCode(t, subnetResp, "validation_failed")

	// PATCH with mtu -> expect 400 validation_failed (mtu is fixed at 1390 for overlay).
	mtuResp := h.patch(t, "/v1/networks/"+created.ID, map[string]any{"mtu": 1500}, admin)
	if mtuResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("patch mtu on overlay status = %d, want 400", mtuResp.StatusCode)
	}
	assertErrorCode(t, mtuResp, "validation_failed")
}

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

// TestOverlayNetworkListFilterAndDistinctVNIs creates two overlay networks and
// one bridge network, then verifies that GET /v1/networks?type=overlay returns
// exactly the two overlay entries, each with a non-nil VNI, and that the two
// VNIs are distinct.
func TestOverlayNetworkListFilterAndDistinctVNIs(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	suffix := uuid.NewString()[:8]

	// Create two overlay networks with distinct subnets.
	ov1Resp := h.post(t, "/v1/networks", map[string]any{
		"name":   "ov-filter-a-" + suffix,
		"type":   "overlay",
		"subnet": "10.54.0.0/24",
	}, admin)
	if ov1Resp.StatusCode != http.StatusCreated {
		t.Fatalf("create overlay-a status = %d, want 201", ov1Resp.StatusCode)
	}
	var ov1 overlayNetworkView
	decodeJSON(t, ov1Resp, &ov1)

	ov2Resp := h.post(t, "/v1/networks", map[string]any{
		"name":   "ov-filter-b-" + suffix,
		"type":   "overlay",
		"subnet": "10.55.0.0/24",
	}, admin)
	if ov2Resp.StatusCode != http.StatusCreated {
		t.Fatalf("create overlay-b status = %d, want 201", ov2Resp.StatusCode)
	}
	var ov2 overlayNetworkView
	decodeJSON(t, ov2Resp, &ov2)

	// Create a bridge network to confirm it does not appear in the filtered list.
	bridgeSuffix := suffix[:6]
	brResp := h.post(t, "/v1/networks", map[string]any{
		"name":        "br-filter-" + suffix,
		"type":        "bridge",
		"bridge_name": "br" + bridgeSuffix,
	}, admin)
	if brResp.StatusCode != http.StatusCreated {
		t.Fatalf("create bridge status = %d, want 201", brResp.StatusCode)
	}
	brResp.Body.Close()

	// GET /v1/networks?type=overlay - must return exactly the two overlays.
	listResp := h.get(t, "/v1/networks?type=overlay", admin)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list overlay status = %d, want 200", listResp.StatusCode)
	}
	var listed struct {
		Data []overlayNetworkView `json:"data"`
	}
	decodeJSON(t, listResp, &listed)

	if len(listed.Data) != 2 {
		t.Fatalf("list overlay count = %d, want 2", len(listed.Data))
	}
	for _, n := range listed.Data {
		if n.Type != "overlay" {
			t.Errorf("listed item %q has type %q, want overlay", n.ID, n.Type)
		}
		if n.VNI == nil {
			t.Errorf("listed overlay %q has nil vni, want allocated VNI", n.ID)
		}
	}

	// Both VNIs must be present and distinct.
	if ov1.VNI == nil || ov2.VNI == nil {
		t.Fatalf("one or both VNIs are nil: ov1.vni=%v ov2.vni=%v", ov1.VNI, ov2.VNI)
	}
	if *ov1.VNI == *ov2.VNI {
		t.Errorf("VNIs are not distinct: ov1.vni=%d ov2.vni=%d", *ov1.VNI, *ov2.VNI)
	}

	// Also confirm bridge type filter does not include overlays.
	bridgeListResp := h.get(t, "/v1/networks?type=bridge", admin)
	if bridgeListResp.StatusCode != http.StatusOK {
		t.Fatalf("list bridge status = %d, want 200", bridgeListResp.StatusCode)
	}
	var bridgeListed struct {
		Data []overlayNetworkView `json:"data"`
	}
	decodeJSON(t, bridgeListResp, &bridgeListed)
	for _, n := range bridgeListed.Data {
		if n.Type == "overlay" {
			t.Errorf("list ?type=bridge returned overlay entry %q", n.ID)
		}
	}
}

// TestOverlayNetworkRBACDeveloperCannotCreate verifies that a developer-role
// principal receives 403 permission_denied when attempting to create a network.
// network:manage is admin/operator-only; developer holds only network:read.
func TestOverlayNetworkRBACDeveloperCannotCreate(t *testing.T) {
	h := newE2E(t)
	dev, _ := loginAs(t, h, auth.RoleDeveloper)

	resp := h.post(t, "/v1/networks", map[string]any{
		"name":   "ov-dev-" + uuid.NewString()[:8],
		"type":   "overlay",
		"subnet": "10.56.0.0/24",
	}, dev)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("developer create overlay status = %d, want 403", resp.StatusCode)
	}
	assertErrorCode(t, resp, "permission_denied")
}

// TestOverlayVMAttachStillRejected is the N3a/N3c slice-boundary guard: an
// overlay network exists in the store (created via the real API) but attaching
// a VM NIC to it must still be refused with 400 until N3c explicitly enables
// it. If this test ever starts returning 202, N3c work landed early.
func TestOverlayVMAttachStillRejected(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)

	// Create an overlay network via the API (not injected through the store).
	ovResp := h.post(t, "/v1/networks", map[string]any{
		"name":   "ov-vmattach-" + uuid.NewString()[:8],
		"type":   "overlay",
		"subnet": "10.57.0.0/24",
	}, admin)
	if ovResp.StatusCode != http.StatusCreated {
		t.Fatalf("create overlay status = %d, want 201", ovResp.StatusCode)
	}
	var ov overlayNetworkView
	decodeJSON(t, ovResp, &ov)

	// Seed a schedulable fixture (node + pool + template) so the pool resolver
	// succeeds and execution reaches the network-type guard. In Handler.Create
	// resolvePoolName runs before resolveNetwork, so without the fixture the
	// request would 404 on the pool before the overlay guard is ever hit.
	poolName, templateName := schedulableFixture(t, h, adminID)

	resp := h.post(t, "/v1/vms", map[string]any{
		"name":      "vm-ovattach-" + uuid.NewString()[:8],
		"template":  templateName,
		"pool":      poolName,
		"vcpus":     2,
		"memory_mb": 2048,
		"network":   ov.Name,
	}, admin)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("attach VM to overlay status = %d, want 400 (overlay attach not yet enabled)", resp.StatusCode)
	}
	assertErrorCode(t, resp, "validation_failed")
}
