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
	"io"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
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
	ID         string `json:"id"`
	Type       string `json:"type"`
	VNI        *int32 `json:"vni"`
	BridgeName string `json:"bridge_name"`
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
		"name":   "ov-dev-" + uuid.NewString()[:8],
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

// TestOverlayNetworkPatchOnlyName verifies that PATCH on an overlay network
// accepts only the name and leaves the server-owned fields (vni, subnet,
// bridge_name) intact across the rename.
func TestOverlayNetworkPatchOnlyName(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	suffix := uuid.NewString()[:8]

	// Create an overlay network.
	createBody := map[string]any{
		"name":   "ov-patch-" + suffix,
		"type":   "overlay",
		"subnet": "10.53.0.0/24",
	}
	createResp := h.post(t, "/v1/networks", createBody, admin)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create overlay status = %d, want 201", createResp.StatusCode)
	}
	var created overlayNetworkView
	decodeJSON(t, createResp, &created)

	if created.VNI == nil {
		t.Fatalf("created overlay has nil VNI")
	}
	origVNI := *created.VNI
	origBridge := created.BridgeName

	// PATCH with only name -> expect 200; verify VNI/bridge/type survive the rename.
	newName := "ov-renamed-" + suffix
	patchResp := h.patch(t, "/v1/networks/"+created.ID, map[string]any{"name": newName}, admin)
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("patch name status = %d, want 200", patchResp.StatusCode)
	}
	var patched overlayNetworkView
	decodeJSON(t, patchResp, &patched)

	if patched.Type != "overlay" {
		t.Errorf("type after rename = %q, want overlay", patched.Type)
	}
	if patched.Name != newName {
		t.Errorf("name after rename = %q, want %q", patched.Name, newName)
	}
	if patched.VNI == nil || *patched.VNI != origVNI {
		t.Errorf("vni after rename = %v, want %d (must not change on rename)", patched.VNI, origVNI)
	}
	if patched.BridgeName != origBridge {
		t.Errorf("bridge_name after rename = %q, want %q (must not change on rename)", patched.BridgeName, origBridge)
	}

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

// TestOverlayNetworkDeclaredToAgents verifies that overlay networks ARE now
// declared in the declared_networks down-channel (the N3b D1 flip). It seeds one
// bridge and one overlay network, drives a synthetic agent heartbeat over the
// mTLS agent router, and asserts that the overlay entry appears carrying its vni
// and otb<vni> bridge name, alongside the bridge.
func TestOverlayNetworkDeclaredToAgents(t *testing.T) {
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
	overlaySeen := false
	for _, n := range decoded.DeclaredNetworks {
		if n.ID == bridgeID {
			bridgeSeen = true
		}
		if n.Type == "overlay" {
			overlaySeen = true
			if n.VNI == nil {
				t.Errorf("declared overlay id=%q has nil vni", n.ID)
			} else if want := fmt.Sprintf("otb%d", *n.VNI); n.BridgeName != want {
				t.Errorf("declared overlay bridge_name = %q, want %q", n.BridgeName, want)
			}
		}
	}
	if !bridgeSeen {
		t.Errorf("declared_networks missing the seeded bridge id=%q", bridgeID)
	}
	if !overlaySeen {
		t.Errorf("declared_networks missing the overlay network; N3b must declare overlay to agents")
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

// TestOverlayVMAttachAllowed is the N3c gate-lift assertion: an overlay network
// created via the real API can be requested as the attach target at VM create
// time. Admission is name-deferred (the NIC is minted at bind), so the create
// reaches 201 pending with the overlay name stashed on the SchedulingSpec.
func TestOverlayVMAttachAllowed(t *testing.T) {
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

	// Seed a schedulable fixture (node + pool + default firmware) so the pool
	// resolver succeeds; placement is deferred to the schedule loop.
	poolName := schedulableFixture(t, h, adminID)

	resp := h.post(t, "/v1/vms", vmCreateBody(map[string]any{
		"pool": poolName, "network": ov.Name,
	}), admin)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("attach VM to overlay status = %d, want 201; body=%s", resp.StatusCode, body)
	}
	var created struct {
		Networks []string `json:"networks"`
	}
	decodeJSON(t, resp, &created)
	if diff := cmp.Diff([]string{ov.Name}, created.Networks); diff != "" {
		t.Errorf("pending networks mismatch (-want +got):\n%s", diff)
	}
}

// TestOverlayNetworkCreateBelowUnderlayFloor409 is the C1 contract assertion:
// when the cluster underlay MTU sits below the 1390 floor (a legacy cluster
// seeded under the old 1280 floor), an overlay network create must refuse with
// 409 conflict carrying the underlay_mtu / min_underlay_mtu / derived_overlay_mtu
// detail keys rather than falling to a generic 500. The sub-floor underlay is
// seeded directly onto the cluster_settings singleton because SeedUnderlayMTU
// rejects below-floor values; this mirrors the store-layer
// TestCreateNetworkOverlayRefusesSubFloorUnderlay over the real HTTP edge.
func TestOverlayNetworkCreateBelowUnderlayFloor409(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	// Seed a sub-floor underlay MTU straight onto the singleton. SeedUnderlayMTU
	// would reject 1300 (< 1390 floor), so write the cluster_settings JSON via the
	// shared client the store wraps, exactly as the etcdstore internal test does
	// through casClusterSettings.
	subFloor := int32(1300)
	cs := store.ClusterSetting{ID: 1, UnderlayMTU: &subFloor}
	if err := sharedEtcdClient.PutJSON(context.Background(),
		etcd.Key("cluster_settings", "singleton"), &cs); err != nil {
		t.Fatalf("seed sub-floor underlay: %v", err)
	}

	resp := h.post(t, "/v1/networks", map[string]any{
		"name":   "ov-subfloor-" + uuid.NewString()[:8],
		"type":   "overlay",
		"subnet": "10.58.0.0/24",
	}, admin)
	if resp.StatusCode != http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create overlay on sub-floor underlay status = %d, want 409; body=%s", resp.StatusCode, body)
	}

	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	decodeJSON(t, resp, &env)

	if env.Error.Code != "conflict" {
		t.Errorf("error.code = %q, want conflict", env.Error.Code)
	}

	// All three detail keys must be present with the derived numeric values.
	// JSON numbers decode into any as float64.
	wantDetails := map[string]float64{
		"underlay_mtu":        float64(subFloor),
		"min_underlay_mtu":    1390,
		"derived_overlay_mtu": float64(subFloor - store.OverlayEncapOverhead),
	}
	for key, want := range wantDetails {
		raw, ok := env.Error.Details[key]
		if !ok {
			t.Errorf("details missing key %q (details=%v)", key, env.Error.Details)
			continue
		}
		got, ok := raw.(float64)
		if !ok {
			t.Errorf("details[%q] = %v (%T), want a number", key, raw, raw)
			continue
		}
		if got != want {
			t.Errorf("details[%q] = %v, want %v", key, got, want)
		}
	}
}
