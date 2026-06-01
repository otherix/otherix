// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

type networkView struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Type       string             `json:"type"`
	BridgeName string             `json:"bridge_name"`
	Managed    bool               `json:"managed"`
	Egress     string             `json:"egress"`
	Subnet     *string            `json:"subnet"`
	Gateway    *string            `json:"gateway"`
	MTU        int32              `json:"mtu"`
	Status     *networkStatusView `json:"status"`
}

type networkStatusView struct {
	Nodes []networkNodeStatusView `json:"nodes"`
}

type networkNodeStatusView struct {
	NodeID               string  `json:"node_id"`
	ReconciliationStatus string  `json:"reconciliation_status"`
	ReconciliationError  *string `json:"reconciliation_error"`
	LastReconciledAt     *string `json:"last_reconciled_at"`
}

func newNetworkBody() map[string]any {
	suffix := uuid.NewString()[:8]
	return map[string]any{
		"name":        "net-" + suffix,
		"type":        "bridge",
		"bridge_name": "br" + suffix[:6],
	}
}

func TestNetworksCRUDAsAdmin(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	resp := h.post(t, "/v1/networks", newNetworkBody(), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created networkView
	decodeJSON(t, resp, &created)
	if created.ID == "" || created.Type != "bridge" {
		t.Fatalf("create view = %+v", created)
	}
	if created.MTU != 1500 {
		t.Errorf("default mtu = %d, want 1500", created.MTU)
	}

	resp = h.get(t, "/v1/networks/"+created.ID, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Patch mtu.
	resp = h.patch(t, "/v1/networks/"+created.ID, map[string]any{"mtu": 9000}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	var patched networkView
	decodeJSON(t, resp, &patched)
	if patched.MTU != 9000 {
		t.Errorf("patched mtu = %d, want 9000", patched.MTU)
	}

	// type is immutable.
	resp = h.patch(t, "/v1/networks/"+created.ID, map[string]any{"type": "vlan"}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("patch type status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.delete(t, "/v1/networks/"+created.ID, admin)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 204/200", resp.StatusCode)
	}
	resp.Body.Close()
}

func assertErrorCode(t *testing.T, resp *http.Response, want string) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeJSON(t, resp, &env)
	if env.Error.Code != want {
		t.Errorf("error code = %q, want %q", env.Error.Code, want)
	}
}

func managedNATBody(t *testing.T) map[string]any {
	t.Helper()
	body := newNetworkBody()
	body["managed"] = true
	body["egress"] = "nat"
	body["subnet"] = "10.20.0.0/24"
	return body
}

func TestNetworksCreateManagedNATDerivesGateway(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	resp := h.post(t, "/v1/networks", managedNATBody(t), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created networkView
	decodeJSON(t, resp, &created)
	if !created.Managed {
		t.Errorf("managed = false, want true")
	}
	if created.Egress != "nat" {
		t.Errorf("egress = %q, want nat", created.Egress)
	}
	if created.Subnet == nil || *created.Subnet != "10.20.0.0/24" {
		t.Errorf("subnet = %v, want 10.20.0.0/24", created.Subnet)
	}
	if created.Gateway == nil || *created.Gateway != "10.20.0.1" {
		t.Errorf("gateway = %v, want 10.20.0.1", created.Gateway)
	}
}

func TestNetworksCreateNATSubnetHostBitsCanonicalised(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	body := managedNATBody(t)
	body["subnet"] = "10.20.0.5/24"
	resp := h.post(t, "/v1/networks", body, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created networkView
	decodeJSON(t, resp, &created)
	if created.Subnet == nil || *created.Subnet != "10.20.0.0/24" {
		t.Errorf("subnet = %v, want canonical 10.20.0.0/24", created.Subnet)
	}
}

func TestNetworksCreateNATWithoutManaged400(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	body := managedNATBody(t)
	body["managed"] = false
	resp := h.post(t, "/v1/networks", body, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create status = %d, want 400", resp.StatusCode)
	}
	assertErrorCode(t, resp, "validation_failed")
}

func TestNetworksCreateNATWithoutSubnet400(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	body := managedNATBody(t)
	delete(body, "subnet")
	resp := h.post(t, "/v1/networks", body, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create status = %d, want 400", resp.StatusCode)
	}
	assertErrorCode(t, resp, "validation_failed")
}

func TestNetworksCreateNoneWithSubnet400(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	body := newNetworkBody()
	body["egress"] = "none"
	body["subnet"] = "10.20.0.0/24"
	resp := h.post(t, "/v1/networks", body, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("create status = %d, want 400", resp.StatusCode)
	}
	assertErrorCode(t, resp, "validation_failed")
}

func TestNetworksUpdateManagedForbidden(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	resp := h.post(t, "/v1/networks", newNetworkBody(), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created networkView
	decodeJSON(t, resp, &created)

	resp = h.patch(t, "/v1/networks/"+created.ID, map[string]any{"managed": true}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("patch managed status = %d, want 400", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				ForbiddenFields []string `json:"forbidden_fields"`
			} `json:"details"`
		} `json:"error"`
	}
	decodeJSON(t, resp, &env)
	if env.Error.Code != "validation_failed" {
		t.Errorf("error code = %q, want validation_failed", env.Error.Code)
	}
	if len(env.Error.Details.ForbiddenFields) != 1 || env.Error.Details.ForbiddenFields[0] != "managed" {
		t.Errorf("forbidden_fields = %v, want [managed]", env.Error.Details.ForbiddenFields)
	}
}

func TestNetworksUpdateEgressNoneToNATDerivesGateway(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	// Create a managed bridge with egress=none so the nat invariant can be met
	// on PATCH (nat requires managed=true, which is immutable post-create).
	body := newNetworkBody()
	body["managed"] = true
	resp := h.post(t, "/v1/networks", body, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created networkView
	decodeJSON(t, resp, &created)

	resp = h.patch(t, "/v1/networks/"+created.ID, map[string]any{
		"egress": "nat",
		"subnet": "192.168.50.0/24",
	}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	var patched networkView
	decodeJSON(t, resp, &patched)
	if patched.Egress != "nat" {
		t.Errorf("egress = %q, want nat", patched.Egress)
	}
	if patched.Subnet == nil || *patched.Subnet != "192.168.50.0/24" {
		t.Errorf("subnet = %v, want 192.168.50.0/24", patched.Subnet)
	}
	if patched.Gateway == nil || *patched.Gateway != "192.168.50.1" {
		t.Errorf("gateway = %v, want 192.168.50.1", patched.Gateway)
	}
}

func TestNetworksGetStatusEmptyThenPopulated(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	resp := h.post(t, "/v1/networks", newNetworkBody(), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created networkView
	decodeJSON(t, resp, &created)

	// Before any heartbeat the status object is present with an empty node set.
	resp = h.get(t, "/v1/networks/"+created.ID, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	var fetched networkView
	decodeJSON(t, resp, &fetched)
	if fetched.Status == nil {
		t.Fatalf("status is nil on get, want present")
	}
	if len(fetched.Status.Nodes) != 0 {
		t.Errorf("status.nodes = %v, want empty", fetched.Status.Nodes)
	}

	// Inject a per-node reconciliation record directly through the store, then
	// confirm the GET-by-id view surfaces it.
	netID := uuid.MustParse(created.ID)
	nodeID := uuid.New()
	if err := h.store.UpsertNetworkNodeStatus(context.Background(), store.UpsertNetworkNodeStatusParams{
		NetworkID:            netID,
		NodeID:               nodeID,
		ReconciliationStatus: "ready",
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus: %v", err)
	}

	resp = h.get(t, "/v1/networks/"+created.ID, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	var withStatus networkView
	decodeJSON(t, resp, &withStatus)
	if withStatus.Status == nil || len(withStatus.Status.Nodes) != 1 {
		t.Fatalf("status.nodes = %+v, want one node", withStatus.Status)
	}
	got := withStatus.Status.Nodes[0]
	if got.NodeID != nodeID.String() {
		t.Errorf("node_id = %q, want %q", got.NodeID, nodeID)
	}
	if got.ReconciliationStatus != "ready" {
		t.Errorf("reconciliation_status = %q, want ready", got.ReconciliationStatus)
	}
}

func TestNetworksListOmitsStatus(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	resp := h.post(t, "/v1/networks", newNetworkBody(), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.get(t, "/v1/networks", admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	var listed struct {
		Data []networkView `json:"data"`
	}
	decodeJSON(t, resp, &listed)
	if len(listed.Data) == 0 {
		t.Fatalf("list returned no networks")
	}
	for _, n := range listed.Data {
		if n.Status != nil {
			t.Errorf("list item %s carries status, want omitted", n.ID)
		}
	}
}

func TestNetworksManageForbiddenForViewer(t *testing.T) {
	h := newE2E(t)
	viewer, _ := loginAs(t, h, auth.RoleViewer)
	// Viewer holds network:read but not network:manage.
	resp := h.post(t, "/v1/networks", newNetworkBody(), viewer)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer create status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// But viewer can list.
	resp = h.get(t, "/v1/networks", viewer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer list status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}
