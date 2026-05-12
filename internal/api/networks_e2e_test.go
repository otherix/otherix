// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// networkBody builds a valid POST /v1/networks payload. Bridge name
// is parameterised so each test gets its own — a new network never
// has an in-use bridge to worry about because nothing actually
// allocates the interface yet.
func networkBody(name string) map[string]any {
	return map[string]any{
		"name":        name,
		"type":        "bridge",
		"bridge_name": "br0",
	}
}

func uniqueNetworkName(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

// createNetworkAsAdmin seeds a network via the API and returns its
// id. Used by tests whose subject is something other than network
// creation.
func createNetworkAsAdmin(t *testing.T, h *e2eHarness, name string) string {
	t.Helper()
	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/networks", networkBody(name), adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed network: status = %d, want 201", resp.StatusCode)
	}
	var v struct {
		ID string `json:"id"`
	}
	decodeJSON(t, resp, &v)
	if v.ID == "" {
		t.Fatal("seed network: empty id in response")
	}
	return v.ID
}

func TestE2E_Networks_CreateRequiresAdmin(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.post(t, "/v1/networks", networkBody(uniqueNetworkName("nope")), tok)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

func TestE2E_Networks_CreateHappyPath(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	name := uniqueNetworkName("happy")
	resp := h.post(t, "/v1/networks", networkBody(name), adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	var view struct {
		ID         string          `json:"id"`
		Name       string          `json:"name"`
		Type       string          `json:"type"`
		BridgeName string          `json:"bridge_name"`
		VlanTag    *int32          `json:"vlan_tag"`
		MTU        int32           `json:"mtu"`
		Config     json.RawMessage `json:"config"`
	}
	decodeJSON(t, resp, &view)

	if view.Name != name {
		t.Errorf("name = %q, want %q", view.Name, name)
	}
	if view.Type != "bridge" {
		t.Errorf("type = %q, want bridge", view.Type)
	}
	if view.BridgeName != "br0" {
		t.Errorf("bridge_name = %q, want br0", view.BridgeName)
	}
	if view.MTU != 1500 {
		t.Errorf("mtu = %d, want 1500 (default)", view.MTU)
	}
	if view.VlanTag != nil {
		t.Errorf("vlan_tag = %v, want nil", *view.VlanTag)
	}
	if string(view.Config) != "{}" {
		t.Errorf("config = %s, want {}", view.Config)
	}
	if _, err := uuid.Parse(view.ID); err != nil {
		t.Errorf("id = %q, not a uuid", view.ID)
	}
}

func TestE2E_Networks_CreateAcceptsJumboMTU(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	body := networkBody(uniqueNetworkName("jumbo"))
	body["mtu"] = 9000
	body["vlan_tag"] = 100

	resp := h.post(t, "/v1/networks", body, adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var view struct {
		MTU     int32  `json:"mtu"`
		VlanTag *int32 `json:"vlan_tag"`
	}
	decodeJSON(t, resp, &view)
	if view.MTU != 9000 {
		t.Errorf("mtu = %d, want 9000", view.MTU)
	}
	if view.VlanTag == nil || *view.VlanTag != 100 {
		t.Errorf("vlan_tag = %v, want 100", view.VlanTag)
	}
}

func TestE2E_Networks_CreateValidation(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	cases := []struct {
		name string
		mut  func(map[string]any)
	}{
		{name: "empty name", mut: func(b map[string]any) { b["name"] = "" }},
		{name: "bad type", mut: func(b map[string]any) { b["type"] = "nat" }},
		{name: "missing bridge_name", mut: func(b map[string]any) { delete(b, "bridge_name") }},
		{name: "bad bridge_name", mut: func(b map[string]any) { b["bridge_name"] = "0bad" }},
		{name: "vlan too low", mut: func(b map[string]any) { b["vlan_tag"] = 0 }},
		{name: "vlan too high", mut: func(b map[string]any) { b["vlan_tag"] = 4095 }},
		{name: "mtu too low", mut: func(b map[string]any) { b["mtu"] = 50 }},
		{name: "mtu too high", mut: func(b map[string]any) { b["mtu"] = 100000 }},
		{name: "config not object", mut: func(b map[string]any) { b["config"] = []int{1, 2, 3} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := networkBody(uniqueNetworkName("v"))
			tc.mut(body)
			resp := h.post(t, "/v1/networks", body, adminToken)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			var b response.ErrorBody
			decodeJSON(t, resp, &b)
			if b.Error.Code != response.CodeValidationFailed {
				t.Errorf("code = %q, want %q", b.Error.Code, response.CodeValidationFailed)
			}
		})
	}
}

func TestE2E_Networks_CreateDuplicateName(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	name := uniqueNetworkName("dup")
	resp1 := h.post(t, "/v1/networks", networkBody(name), adminToken)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", resp1.StatusCode)
	}
	resp2 := h.post(t, "/v1/networks", networkBody(name), adminToken)
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("dup status = %d, want 409", resp2.StatusCode)
	}
}

func TestE2E_Networks_GetAndListAllRolesCanRead(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createNetworkAsAdmin(t, h, uniqueNetworkName("read"))
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.get(t, "/v1/networks/"+id, tok)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("get status = %d, want 200", resp.StatusCode)
			}
			var view map[string]any
			decodeJSON(t, resp, &view)
			for _, k := range []string{"id", "name", "type", "bridge_name", "mtu", "config", "created_at", "updated_at"} {
				if _, ok := view[k]; !ok {
					t.Errorf("missing %q in view", k)
				}
			}
			respList := h.get(t, "/v1/networks", tok)
			if respList.StatusCode != http.StatusOK {
				t.Errorf("list status = %d, want 200", respList.StatusCode)
			}
		})
	}
}

func TestE2E_Networks_GetUnknown404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/networks/"+uuid.NewString(), adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_Networks_ListByType(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	createNetworkAsAdmin(t, h, uniqueNetworkName("type-bridge"))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	resp := h.get(t, "/v1/networks?type=bridge", adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var page struct {
		Data []map[string]any `json:"data"`
	}
	decodeJSON(t, resp, &page)
	if len(page.Data) == 0 {
		t.Errorf("empty data, want >=1 bridge network")
	}

	respBad := h.get(t, "/v1/networks?type=invalid", adminToken)
	if respBad.StatusCode != http.StatusBadRequest {
		t.Errorf("bad-type status = %d, want 400", respBad.StatusCode)
	}
}

func TestE2E_Networks_UpdateRequiresAdmin(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createNetworkAsAdmin(t, h, uniqueNetworkName("upd-rbac"))
	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.patch(t, "/v1/networks/"+id, map[string]any{"name": "new"}, tok, "")
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

func TestE2E_Networks_UpdateMutableFields(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createNetworkAsAdmin(t, h, uniqueNetworkName("upd"))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	newName := uniqueNetworkName("renamed")
	body := map[string]any{
		"name":        newName,
		"bridge_name": "br-lan",
		"vlan_tag":    42,
		"mtu":         9000,
		"config":      map[string]any{"comment": "lab"},
	}
	resp := h.patch(t, "/v1/networks/"+id, body, adminToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var view struct {
		Name       string          `json:"name"`
		BridgeName string          `json:"bridge_name"`
		VlanTag    *int32          `json:"vlan_tag"`
		MTU        int32           `json:"mtu"`
		Config     json.RawMessage `json:"config"`
	}
	decodeJSON(t, resp, &view)
	if view.Name != newName {
		t.Errorf("name = %q, want %q", view.Name, newName)
	}
	if view.BridgeName != "br-lan" {
		t.Errorf("bridge_name = %q, want br-lan", view.BridgeName)
	}
	if view.VlanTag == nil || *view.VlanTag != 42 {
		t.Errorf("vlan_tag = %v, want 42", view.VlanTag)
	}
	if view.MTU != 9000 {
		t.Errorf("mtu = %d, want 9000", view.MTU)
	}
}

func TestE2E_Networks_UpdateClearVlanTag(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	body := networkBody(uniqueNetworkName("vlan-clear"))
	body["vlan_tag"] = 100
	resp := h.post(t, "/v1/networks", body, adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID      string `json:"id"`
		VlanTag *int32 `json:"vlan_tag"`
	}
	decodeJSON(t, resp, &created)
	if created.VlanTag == nil || *created.VlanTag != 100 {
		t.Fatalf("setup vlan_tag = %v, want 100", created.VlanTag)
	}

	// Explicit null clears the tag.
	respUpd := h.patch(t, "/v1/networks/"+created.ID,
		map[string]any{"vlan_tag": nil}, adminToken, "")
	if respUpd.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, want 200", respUpd.StatusCode)
	}
	var updated struct {
		VlanTag *int32 `json:"vlan_tag"`
	}
	decodeJSON(t, respUpd, &updated)
	if updated.VlanTag != nil {
		t.Errorf("vlan_tag = %v, want nil", *updated.VlanTag)
	}
}

func TestE2E_Networks_UpdateRejectsImmutableType(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createNetworkAsAdmin(t, h, uniqueNetworkName("upd-type"))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	resp := h.patch(t, "/v1/networks/"+id,
		map[string]any{"type": "bridge"}, adminToken, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeValidationFailed {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeValidationFailed)
	}
	fields, _ := b.Error.Details["forbidden_fields"].([]any)
	if len(fields) == 0 || fields[0] != "type" {
		t.Errorf("forbidden_fields = %v, want [\"type\"]", fields)
	}
}

func TestE2E_Networks_UpdateRenameCollision(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nameA := uniqueNetworkName("a")
	nameB := uniqueNetworkName("b")
	createNetworkAsAdmin(t, h, nameA)
	idB := createNetworkAsAdmin(t, h, nameB)
	adminToken := loginAs(t, h, auth.RoleAdmin)

	resp := h.patch(t, "/v1/networks/"+idB,
		map[string]any{"name": nameA}, adminToken, "")
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409", resp.StatusCode)
	}
}

func TestE2E_Networks_UpdateRejectsBelowMinMTU(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createNetworkAsAdmin(t, h, uniqueNetworkName("mtu-min"))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	resp := h.patch(t, "/v1/networks/"+id,
		map[string]any{"mtu": 50}, adminToken, "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestE2E_Networks_DeleteRequiresAdmin(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createNetworkAsAdmin(t, h, uniqueNetworkName("del-rbac"))
	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.delete(t, "/v1/networks/"+id, tok)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

func TestE2E_Networks_DeleteHappyPath(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createNetworkAsAdmin(t, h, uniqueNetworkName("del-happy"))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	resp := h.delete(t, "/v1/networks/"+id, adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	resp = h.get(t, "/v1/networks/"+id, adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("post-delete get = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_Networks_DeleteUnknown404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/networks/"+uuid.NewString(), adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_Networks_DeleteBlockedByVMNic(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	idStr := createNetworkAsAdmin(t, h, uniqueNetworkName("del-blocked"))
	netID := uuid.MustParse(idStr)

	ownerID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	seedVMNicOnNetwork(t, ctx, h.store, ownerID, netID)

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/networks/"+idStr, adminToken)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeConflict {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeConflict)
	}
	br, _ := b.Error.Details["blocking_resources"].(map[string]any)
	if br == nil {
		t.Fatalf("missing blocking_resources in details: %+v", b.Error.Details)
	}
	if nics, _ := br["vm_nics"].(float64); nics < 1 {
		t.Errorf("blocking_resources.vm_nics = %v, want >=1", br["vm_nics"])
	}
}

func TestE2E_Networks_IdempotencyReplay(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	body := networkBody(uniqueNetworkName("idem"))
	key := "idem-" + uuid.NewString()

	resp1 := h.postIdem(t, "/v1/networks", body, adminToken, key)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", resp1.StatusCode)
	}
	var first map[string]any
	decodeJSON(t, resp1, &first)

	resp2 := h.postIdem(t, "/v1/networks", body, adminToken, key)
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("replay status = %d, want 201", resp2.StatusCode)
	}
	var second map[string]any
	decodeJSON(t, resp2, &second)
	if first["id"] != second["id"] {
		t.Errorf("ids differ on replay: %v vs %v", first["id"], second["id"])
	}
}

func TestE2E_Networks_IdempotencyMismatch(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	bodyA := networkBody(uniqueNetworkName("idem-a"))
	bodyB := networkBody(uniqueNetworkName("idem-b"))
	key := "idem-" + uuid.NewString()

	resp1 := h.postIdem(t, "/v1/networks", bodyA, adminToken, key)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", resp1.StatusCode)
	}
	resp2 := h.postIdem(t, "/v1/networks", bodyB, adminToken, key)
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("mismatch status = %d, want 409", resp2.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp2, &b)
	if b.Error.Code != response.CodeIdempotencyMismatch {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeIdempotencyMismatch)
	}
}

// seedVMNicOnNetwork inserts a vms row plus a single vm_nics row
// referencing networkID. The vm carries the minimum NOT NULL columns
// required by the schema.
func seedVMNicOnNetwork(t *testing.T, ctx context.Context, s *store.Store, ownerID, networkID uuid.UUID) uuid.UUID {
	t.Helper()
	vmID := uuid.New()
	const insVM = `
		insert into vms
		  (id, owner_id, name, architecture, cpu_cores, memory_mib, machine_type)
		values
		  ($1, $2, $3, 'amd64', 1, 256, 'pc-i440fx-8.0')`
	if _, err := s.Pool().Exec(ctx, insVM, vmID, ownerID, "vm-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("insert vms: %v", err)
	}
	const insNIC = `
		insert into vm_nics
		  (id, vm_id, network_id, device_order, mac_address)
		values
		  ($1, $2, $3, 0, '52:54:00:12:34:56'::macaddr)`
	if _, err := s.Pool().Exec(ctx, insNIC, uuid.New(), vmID, networkID); err != nil {
		t.Fatalf("insert vm_nics: %v", err)
	}
	return vmID
}
