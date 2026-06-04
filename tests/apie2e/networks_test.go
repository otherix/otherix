// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"encoding/json"
	"io"
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
	NodeName             string  `json:"node_name"`
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

	// type is immutable: 400 validation_failed with forbidden_fields: ["type"].
	resp = h.patch(t, "/v1/networks/"+created.ID, map[string]any{"type": "vlan"}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("patch type status = %d, want 400", resp.StatusCode)
	}
	var typeEnv struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				ForbiddenFields []string `json:"forbidden_fields"`
			} `json:"details"`
		} `json:"error"`
	}
	decodeJSON(t, resp, &typeEnv)
	if typeEnv.Error.Code != "validation_failed" {
		t.Errorf("patch type error code = %q, want validation_failed", typeEnv.Error.Code)
	}
	if len(typeEnv.Error.Details.ForbiddenFields) != 1 || typeEnv.Error.Details.ForbiddenFields[0] != "type" {
		t.Errorf("patch type forbidden_fields = %v, want [type]", typeEnv.Error.Details.ForbiddenFields)
	}

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

	// Seed a real node and inject a per-node reconciliation record for it
	// directly through the store, then confirm the GET-by-id view surfaces the
	// row keyed by the node id AND carrying the resolved node name.
	netID := uuid.MustParse(created.ID)
	ctx := context.Background()
	nodeID := uuid.New()
	nodeName := "node-" + uuid.NewString()[:8]
	if _, err := h.store.CreateNode(ctx, store.CreateNodeParams{
		ID: nodeID, Name: nodeName, Architecture: store.CpuArchAmd64,
		AdvertisedEndpoint: "https://node.test:9443", MigrationHost: "10.0.0.1",
		MigrationPortRangeStart: 49152, MigrationPortRangeEnd: 49251, Status: store.NodeStatusPending,
	}); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if err := h.store.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
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
	if got.NodeName != nodeName {
		t.Errorf("node_name = %q, want %q", got.NodeName, nodeName)
	}
	if got.ReconciliationStatus != "ready" {
		t.Errorf("reconciliation_status = %q, want ready", got.ReconciliationStatus)
	}

	// A status row for a node that no longer exists (or never did) must not
	// 500 the read: node_id stays present, node_name resolves to empty.
	ghostID := uuid.New()
	if err := h.store.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
		NetworkID:            netID,
		NodeID:               ghostID,
		ReconciliationStatus: "pending",
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus (ghost): %v", err)
	}
	resp = h.get(t, "/v1/networks/"+created.ID, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	var withGhost networkView
	decodeJSON(t, resp, &withGhost)
	if withGhost.Status == nil || len(withGhost.Status.Nodes) != 2 {
		t.Fatalf("status.nodes = %+v, want two nodes", withGhost.Status)
	}
	var ghost *networkNodeStatusView
	for i := range withGhost.Status.Nodes {
		if withGhost.Status.Nodes[i].NodeID == ghostID.String() {
			ghost = &withGhost.Status.Nodes[i]
		}
	}
	if ghost == nil {
		t.Fatalf("ghost node %s not found in status.nodes", ghostID)
	}
	if ghost.NodeName != "" {
		t.Errorf("ghost node_name = %q, want empty", ghost.NodeName)
	}
}

// TestNetworksGetStatusDeletedNodeNameNull verifies that a status row for a
// node that no longer exists serialises node_name as JSON null (the schema's
// nullable form), not an empty string, while node_id stays present and the
// read returns 200.
func TestNetworksGetStatusDeletedNodeNameNull(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	resp := h.post(t, "/v1/networks", newNetworkBody(), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created networkView
	decodeJSON(t, resp, &created)

	netID := uuid.MustParse(created.ID)
	ctx := context.Background()
	ghostID := uuid.New()
	if err := h.store.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
		NetworkID: netID, NodeID: ghostID, ReconciliationStatus: "pending",
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus (ghost): %v", err)
	}

	resp = h.get(t, "/v1/networks/"+created.ID, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	// Decode loosely so node_name can be inspected as a raw JSON value
	// (null vs ""), which the typed view's *string cannot distinguish.
	var body struct {
		Status struct {
			Nodes []struct {
				NodeID   string          `json:"node_id"`
				NodeName json.RawMessage `json:"node_name"`
			} `json:"nodes"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	var ghost *struct {
		NodeID   string          `json:"node_id"`
		NodeName json.RawMessage `json:"node_name"`
	}
	for i := range body.Status.Nodes {
		if body.Status.Nodes[i].NodeID == ghostID.String() {
			ghost = &body.Status.Nodes[i]
		}
	}
	if ghost == nil {
		t.Fatalf("ghost node %s not found in status.nodes", ghostID)
	}
	if string(ghost.NodeName) != "null" {
		t.Errorf("node_name raw = %q, want null", string(ghost.NodeName))
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

// TestNetworksUpdateEgressEmptyNormalisesToNone verifies that a PATCH carrying
// egress="" (an out-of-enum empty string) is normalised to "none" rather than
// persisted verbatim, clearing subnet/gateway per the none-invariant.
func TestNetworksUpdateEgressEmptyNormalisesToNone(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	// Seed a managed nat network with a subnet/gateway.
	resp := h.post(t, "/v1/networks", managedNATBody(t), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created networkView
	decodeJSON(t, resp, &created)
	if created.Egress != "nat" || created.Subnet == nil || created.Gateway == nil {
		t.Fatalf("seed nat network = %+v, want nat with subnet/gateway", created)
	}

	resp = h.patch(t, "/v1/networks/"+created.ID, map[string]any{"egress": ""}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	var patched networkView
	decodeJSON(t, resp, &patched)
	if patched.Egress != "none" {
		t.Errorf("egress = %q, want none", patched.Egress)
	}
	if patched.Subnet != nil {
		t.Errorf("subnet = %v, want nil after egress cleared", patched.Subnet)
	}
	if patched.Gateway != nil {
		t.Errorf("gateway = %v, want nil after egress cleared", patched.Gateway)
	}

	// GET echoes the normalised egress.
	resp = h.get(t, "/v1/networks/"+created.ID, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	var fetched networkView
	decodeJSON(t, resp, &fetched)
	if fetched.Egress != "none" {
		t.Errorf("get egress = %q, want none", fetched.Egress)
	}
}
