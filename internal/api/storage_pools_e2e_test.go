// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// poolBody builds a minimal POST /v1/storage-pools payload bound to
// the supplied node. The path is parameterised by name so each test
// gets a unique on-disk location. Pool names are cluster-wide UNIQUE
// on lower(name); `node` accepts a UUID literal or a node name.
func poolBody(nodeID, name string) map[string]any {
	return map[string]any{
		"node": nodeID,
		"name": name,
		"type": "local_dir",
		"path": "/opt/otherix/pools/" + name,
	}
}

func uniquePoolNameE2E(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

// seedNodeForE2E creates a node row directly through the store (skip
// the create-node API to avoid permission scaffolding) and returns
// the operator-facing name. `/v1/nodes/{name}` is name-only; callers
// building path URLs use the returned name directly. Pool bodies pass
// the name as the `node` body field.
func seedNodeForE2E(t *testing.T, ctx context.Context, s *store.Store) string {
	t.Helper()
	id := uuid.New()
	name := "e2e-node-" + uuid.NewString()[:8]
	if _, err := s.Queries().CreateNode(ctx, store.CreateNodeParams{
		ID:                      id,
		Name:                    name,
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://" + name + ".otherix.local:9443",
		MigrationHost:           "10.0.0.1",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  store.NodeStatusPending,
	}); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return name
}

// putJSON sends PUT against the e2e harness's HTTP surface. Mirrors
// h.post / h.patch — there is no h.put on the shared harness today,
// and this helper stays scoped to cluster endpoint tests rather than
// promoting to the common surface until a second caller appears.
func putJSON(t *testing.T, h *e2eHarness, path string, body any, bearer string) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, h.srv.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// clearDefaultPoolE2E clears the cluster default-pool reference from
// cluster_settings (if set). The cluster default-pool semantic lives
// in the cluster_settings singleton (not in a per-row is_default
// flag); tests that exercise default-pool behaviour against a shared
// harness must clear the slot first to avoid bleed-through from
// earlier tests.
func clearDefaultPoolE2E(t *testing.T, h *e2eHarness) {
	t.Helper()
	ctx := context.Background()
	if err := h.store.Queries().ClearDefaultPoolName(ctx); err != nil {
		t.Fatalf("clearDefaultPoolE2E: %v", err)
	}
}

// createPoolAsAdmin seeds a pool via the API and returns its id.
func createPoolAsAdmin(t *testing.T, h *e2eHarness, nodeID, name string) string {
	t.Helper()
	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/storage-pools", poolBody(nodeID, name), adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed pool: status = %d, want 201", resp.StatusCode)
	}
	var v struct {
		ID string `json:"id"`
	}
	decodeJSON(t, resp, &v)
	if v.ID == "" {
		t.Fatal("seed pool: empty id in response")
	}
	return v.ID
}

func TestE2E_StoragePools_CreateRequiresAdmin(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.post(t, "/v1/storage-pools", poolBody(nodeID, uniquePoolNameE2E("nope")), tok)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

func TestE2E_StoragePools_CreateHappyPath(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	adminToken := loginAs(t, h, auth.RoleAdmin)
	name := uniquePoolNameE2E("happy")

	resp := h.post(t, "/v1/storage-pools", poolBody(nodeID, name), adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var view struct {
		ID             string          `json:"id"`
		Node           string          `json:"node"`
		Name           string          `json:"name"`
		Type           string          `json:"type"`
		Path           string          `json:"path"`
		CapacityBytes  *int64          `json:"capacity_bytes"`
		AvailableBytes *int64          `json:"available_bytes"`
		ReportedAt     *string         `json:"reported_at"`
		IsDefault      bool            `json:"is_default"`
		Config         json.RawMessage `json:"config"`
	}
	decodeJSON(t, resp, &view)

	// The response field carries the node *name*; the request body
	// uses the same narrowing, so the seedNodeForE2E helper returns
	// a name directly. The view's `node` must echo the same name we
	// POSTed (round-trip preservation).
	if view.Node != nodeID {
		t.Errorf("node = %q, want %q (seed helper returns the name)", view.Node, nodeID)
	}
	if view.Name != name {
		t.Errorf("name = %q, want %q", view.Name, name)
	}
	if view.Type != "local_dir" {
		t.Errorf("type = %q, want local_dir", view.Type)
	}
	if view.Path != "/opt/otherix/pools/"+name {
		t.Errorf("path = %q, want /opt/otherix/pools/%s", view.Path, name)
	}
	if view.IsDefault {
		t.Error("is_default = true, want false")
	}
	if view.CapacityBytes != nil || view.AvailableBytes != nil || view.ReportedAt != nil {
		t.Errorf("agent-reported fields surfaced on create: cap=%v avail=%v reported=%v",
			view.CapacityBytes, view.AvailableBytes, view.ReportedAt)
	}
	if string(view.Config) != "{}" {
		t.Errorf("config = %s, want {}", view.Config)
	}
	if _, err := uuid.Parse(view.ID); err != nil {
		t.Errorf("id = %q, not a uuid", view.ID)
	}
}

func TestE2E_StoragePools_CreateRejectsAgentReportedFields(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	adminToken := loginAs(t, h, auth.RoleAdmin)

	for _, field := range []string{"capacity_bytes", "available_bytes", "reported_at"} {
		t.Run(field, func(t *testing.T) {
			body := poolBody(nodeID, uniquePoolNameE2E("forb"))
			switch field {
			case "reported_at":
				body[field] = "2026-05-06T00:00:00Z"
			default:
				body[field] = 42
			}
			resp := h.post(t, "/v1/storage-pools", body, adminToken)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var b response.ErrorBody
			decodeJSON(t, resp, &b)
			if b.Error.Code != response.CodeValidationFailed {
				t.Errorf("code = %q, want %q", b.Error.Code, response.CodeValidationFailed)
			}
			fields, _ := b.Error.Details["forbidden_fields"].([]any)
			if len(fields) == 0 || fields[0] != field {
				t.Errorf("forbidden_fields = %v, want [%q]", fields, field)
			}
		})
	}
}

func TestE2E_StoragePools_CreateValidation(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	adminToken := loginAs(t, h, auth.RoleAdmin)
	cases := []struct {
		name string
		mut  func(map[string]any)
	}{
		{name: "empty name", mut: func(b map[string]any) { b["name"] = "" }},
		{name: "name leading space", mut: func(b map[string]any) { b["name"] = " bad" }},
		{name: "bad type", mut: func(b map[string]any) { b["type"] = "ceph_rbd" }},
		{name: "missing path", mut: func(b map[string]any) { delete(b, "path") }},
		{name: "relative path", mut: func(b map[string]any) { b["path"] = "var/lib" }},
		{name: "tilde path", mut: func(b map[string]any) { b["path"] = "~/pool" }},
		{name: "config not object", mut: func(b map[string]any) { b["config"] = []int{1, 2, 3} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := poolBody(nodeID, uniquePoolNameE2E("v"))
			tc.mut(body)
			resp := h.post(t, "/v1/storage-pools", body, adminToken)
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

// TestE2E_StoragePools_UnknownNodeName404 covers the name-based
// references: passing a non-UUID, non-existent value for `node`
// resolves to a name lookup that returns 404 (no leak).
func TestE2E_StoragePools_UnknownNodeName404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	body := poolBody("not-a-uuid", uniquePoolNameE2E("nn"))
	resp := h.post(t, "/v1/storage-pools", body, adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_StoragePools_CreateUnknownNode404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/storage-pools",
		poolBody("no-such-node-name", uniquePoolNameE2E("ghost")), adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_StoragePools_CreateDuplicateName(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	adminToken := loginAs(t, h, auth.RoleAdmin)
	name := uniquePoolNameE2E("dup")

	resp1 := h.post(t, "/v1/storage-pools", poolBody(nodeID, name), adminToken)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", resp1.StatusCode)
	}
	resp2 := h.post(t, "/v1/storage-pools", poolBody(nodeID, name), adminToken)
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("dup status = %d, want 409", resp2.StatusCode)
	}
}

// TestE2E_StoragePools_CreateRejectsIsDefaultField locks in the
// rule: there is no per-row `is_default` flag; cluster default-pool
// lives in cluster_settings and is configured via PUT
// /v1/cluster/default-pool. A POST body that still carries
// `is_default` returns 400 forbidden_fields.
func TestE2E_StoragePools_CreateRejectsIsDefaultField(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	adminToken := loginAs(t, h, auth.RoleAdmin)

	body := poolBody(nodeID, uniquePoolNameE2E("legacy-default"))
	body["is_default"] = true
	resp := h.post(t, "/v1/storage-pools", body, adminToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 forbidden_fields", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeValidationFailed {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeValidationFailed)
	}
	fields, _ := b.Error.Details["forbidden_fields"].([]any)
	found := false
	for _, f := range fields {
		if f == "is_default" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("forbidden_fields = %v, want to contain is_default", fields)
	}
}

func TestE2E_StoragePools_GetAndListAllRolesCanRead(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	id := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("read"))
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.get(t, "/v1/storage-pools/"+id, tok)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("get status = %d, want 200", resp.StatusCode)
			}
			var view map[string]any
			decodeJSON(t, resp, &view)
			for _, k := range []string{"id", "node", "name", "type", "path", "is_cluster_default", "config", "created_at", "updated_at"} {
				if _, ok := view[k]; !ok {
					t.Errorf("missing %q in view", k)
				}
			}

			respList := h.get(t, "/v1/storage-pools", tok)
			if respList.StatusCode != http.StatusOK {
				t.Errorf("list status = %d, want 200", respList.StatusCode)
			}
		})
	}
}

func TestE2E_StoragePools_GetUnknown404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/storage-pools/"+uuid.NewString(), adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_StoragePools_ListByNodeAndType(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	nodeID := seedNodeForE2E(t, ctx, h.store)
	createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("flt"))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	resp := h.get(t, "/v1/storage-pools?node="+nodeID+"&type=local_dir", adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var page struct {
		Data []map[string]any `json:"data"`
	}
	decodeJSON(t, resp, &page)
	if len(page.Data) == 0 {
		t.Errorf("empty data, want >=1 pool for the seeded node")
	}
	// Response narrows the owning-node field to its name, not the
	// UUID. The handler already filtered by node_id at the SQL
	// layer; we just spot-check the wire shape and trust the query.
	for _, row := range page.Data {
		if _, ok := row["node"].(string); !ok {
			t.Errorf("pool row missing 'node' name field: %v", row)
		}
	}

	respBad := h.get(t, "/v1/storage-pools?type=invalid", adminToken)
	if respBad.StatusCode != http.StatusBadRequest {
		t.Errorf("bad-type status = %d, want 400", respBad.StatusCode)
	}
}

func TestE2E_StoragePools_UpdateRequiresAdmin(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	id := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("upd-rbac"))
	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.patch(t, "/v1/storage-pools/"+id,
				map[string]any{"name": "renamed"}, tok, "")
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

func TestE2E_StoragePools_UpdateMutableFields(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	id := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("upd"))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	newName := uniquePoolNameE2E("renamed")
	body := map[string]any{
		"name":   newName,
		"config": map[string]any{"hint": "warm"},
	}
	resp := h.patch(t, "/v1/storage-pools/"+id, body, adminToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var view struct {
		Name   string          `json:"name"`
		Config json.RawMessage `json:"config"`
	}
	decodeJSON(t, resp, &view)
	if view.Name != newName {
		t.Errorf("name = %q, want %q", view.Name, newName)
	}
}

func TestE2E_StoragePools_UpdateRejectsImmutableFields(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	id := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("upd-imm"))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	cases := []struct {
		field string
		value any
	}{
		{field: "node", value: uuid.NewString()},
		{field: "node_id", value: uuid.NewString()},
		{field: "type", value: "local_dir"},
		{field: "path", value: "/somewhere/else"},
		{field: "is_default", value: true},
		{field: "capacity_bytes", value: 42},
		{field: "available_bytes", value: 7},
		{field: "reported_at", value: "2026-05-06T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			resp := h.patch(t, "/v1/storage-pools/"+id,
				map[string]any{tc.field: tc.value}, adminToken, "")
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var b response.ErrorBody
			decodeJSON(t, resp, &b)
			if b.Error.Code != response.CodeValidationFailed {
				t.Errorf("code = %q, want %q", b.Error.Code, response.CodeValidationFailed)
			}
			fields, _ := b.Error.Details["forbidden_fields"].([]any)
			if len(fields) == 0 || fields[0] != tc.field {
				t.Errorf("forbidden_fields = %v, want [%q]", fields, tc.field)
			}
		})
	}
}

// TestE2E_StoragePools_ClusterDefaultRoundTrip exercises the cluster
// default-pool endpoints through the e2e harness: GET returns 404
// default_pool_not_set when unset; PUT with an unknown name returns 400
// pool_not_found; PUT with a seeded name succeeds; subsequent GET
// returns the persisted name; the per-instance flat view surfaces
// `is_cluster_default = true` for instances whose name matches the
// reference; DELETE clears.
func TestE2E_StoragePools_ClusterDefaultRoundTrip(t *testing.T) {
	h := newE2E(t)
	defer h.close()
	clearDefaultPoolE2E(t, h)

	adminToken := loginAs(t, h, auth.RoleAdmin)
	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	name := uniquePoolNameE2E("clu-def")
	id := createPoolAsAdmin(t, h, nodeID, name)

	// Initial GET: 404.
	resp := h.get(t, "/v1/cluster/default-pool", adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("initial GET status = %d, want 404", resp.StatusCode)
	}

	// PUT with unknown name: 400 pool_not_found.
	resp = putJSON(t, h, "/v1/cluster/default-pool", map[string]any{"name": "no-such-pool"}, adminToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PUT unknown status = %d, want 400", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodePoolNotFound {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodePoolNotFound)
	}

	// PUT with seeded name: 200 + canonical echo.
	resp = putJSON(t, h, "/v1/cluster/default-pool", map[string]any{"name": name}, adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT happy status = %d, want 200", resp.StatusCode)
	}
	var setView struct {
		Name string `json:"name"`
	}
	decodeJSON(t, resp, &setView)
	if setView.Name != name {
		t.Errorf("PUT echo name = %q, want %q", setView.Name, name)
	}

	// Per-instance view reflects the cluster default flag.
	resp = h.get(t, "/v1/storage-pools/"+id, adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("instance GET status = %d, want 200", resp.StatusCode)
	}
	var instanceView struct {
		IsClusterDefault bool `json:"is_cluster_default"`
	}
	decodeJSON(t, resp, &instanceView)
	if !instanceView.IsClusterDefault {
		t.Error("is_cluster_default = false, want true after PUT")
	}

	// DELETE clears.
	resp = h.delete(t, "/v1/cluster/default-pool", adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", resp.StatusCode)
	}
	resp = h.get(t, "/v1/cluster/default-pool", adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("post-clear GET status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_StoragePools_DeleteRequiresAdmin(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	id := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("del-rbac"))
	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.delete(t, "/v1/storage-pools/"+id, tok)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

func TestE2E_StoragePools_DeleteHappyPath(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	id := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("del-happy"))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	resp := h.delete(t, "/v1/storage-pools/"+id, adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	resp = h.get(t, "/v1/storage-pools/"+id, adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("post-delete get = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_StoragePools_DeleteUnknown404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/storage-pools/"+uuid.NewString(), adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_StoragePools_DeleteBlockedByVMDisk(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	nodeID := seedNodeForE2E(t, ctx, h.store)
	idStr := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("del-blocked"))
	poolID := uuid.MustParse(idStr)

	ownerID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	seedVMDiskOnPool(t, ctx, h.store, ownerID, poolID)

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/storage-pools/"+idStr, adminToken)
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
	if disks, _ := br["vm_disks"].(float64); disks < 1 {
		t.Errorf("blocking_resources.vm_disks = %v, want >=1", br["vm_disks"])
	}
	if _, hasImages := br["storage_images"]; hasImages {
		t.Errorf("blocking_resources.storage_images present = %v, want absent (no images in this scenario)", br["storage_images"])
	}
}

// TestE2E_StoragePools_DeleteBlockedByStorageImages locks in the rule
// that a pool with materialised storage images refuses delete with
// the dedicated `pool_in_use` code, even when no vm_disks reference
// it.
func TestE2E_StoragePools_DeleteBlockedByStorageImages(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	nodeID := seedNodeForE2E(t, ctx, h.store)
	poolStr := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("img-only"))
	poolID := uuid.MustParse(poolStr)

	ownerID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	fwID := seedFirmwareForImagesE2E(t, ctx, h.store)
	tplID, _ := seedTemplateOnFirmware(t, ctx, h.store, ownerID, fwID)
	seedStorageImage(t, ctx, h.store, tplID, poolID, imageSHA256E2E(0xab), 1024, "qcow2")

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/storage-pools/"+poolStr, adminToken)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeResourceInUse {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeResourceInUse)
	}
	if got, _ := b.Error.Details["kind"].(string); got != "pool" {
		t.Errorf("details.kind = %v, want \"pool\"", b.Error.Details["kind"])
	}
	br, _ := b.Error.Details["blocking_resources"].(map[string]any)
	if br == nil {
		t.Fatalf("missing blocking_resources in details: %+v", b.Error.Details)
	}
	if got, _ := br["storage_images"].(float64); got < 1 {
		t.Errorf("blocking_resources.storage_images = %v, want >=1", br["storage_images"])
	}
	if _, hasDisks := br["vm_disks"]; hasDisks {
		t.Errorf("blocking_resources.vm_disks present = %v, want absent (no disks in this scenario)", br["vm_disks"])
	}
}

// TestE2E_StoragePools_DeleteBlockedMultiResource asserts the
// multi-resource envelope when a pool is blocked by BOTH vm_disks and
// storage_images. Stacked refusal carries the dedicated `pool_in_use`
// code and both keys appear in the blocking-resources map.
func TestE2E_StoragePools_DeleteBlockedMultiResource(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	nodeID := seedNodeForE2E(t, ctx, h.store)
	poolStr := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("multi"))
	poolID := uuid.MustParse(poolStr)

	ownerID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	seedVMDiskOnPool(t, ctx, h.store, ownerID, poolID)

	fwID := seedFirmwareForImagesE2E(t, ctx, h.store)
	tplID, _ := seedTemplateOnFirmware(t, ctx, h.store, ownerID, fwID)
	seedStorageImage(t, ctx, h.store, tplID, poolID, imageSHA256E2E(0xcd), 2048, "qcow2")

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/storage-pools/"+poolStr, adminToken)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeResourceInUse {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeResourceInUse)
	}
	if got, _ := b.Error.Details["kind"].(string); got != "pool" {
		t.Errorf("details.kind = %v, want \"pool\"", b.Error.Details["kind"])
	}
	br, _ := b.Error.Details["blocking_resources"].(map[string]any)
	if br == nil {
		t.Fatalf("missing blocking_resources: %+v", b.Error.Details)
	}
	if got, _ := br["storage_images"].(float64); got < 1 {
		t.Errorf("blocking_resources.storage_images = %v, want >=1", br["storage_images"])
	}
	if got, _ := br["vm_disks"].(float64); got < 1 {
		t.Errorf("blocking_resources.vm_disks = %v, want >=1", br["vm_disks"])
	}
}

func TestE2E_StoragePools_IdempotencyReplay(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	adminToken := loginAs(t, h, auth.RoleAdmin)
	body := poolBody(nodeID, uniquePoolNameE2E("idem"))
	key := "idem-" + uuid.NewString()

	resp1 := h.postIdem(t, "/v1/storage-pools", body, adminToken, key)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", resp1.StatusCode)
	}
	var first map[string]any
	decodeJSON(t, resp1, &first)

	resp2 := h.postIdem(t, "/v1/storage-pools", body, adminToken, key)
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("replay status = %d, want 201", resp2.StatusCode)
	}
	var second map[string]any
	decodeJSON(t, resp2, &second)
	if first["id"] != second["id"] {
		t.Errorf("ids differ on replay: %v vs %v", first["id"], second["id"])
	}
}

func TestE2E_StoragePools_IdempotencyMismatch(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	adminToken := loginAs(t, h, auth.RoleAdmin)
	bodyA := poolBody(nodeID, uniquePoolNameE2E("idem-a"))
	bodyB := poolBody(nodeID, uniquePoolNameE2E("idem-b"))
	key := "idem-" + uuid.NewString()

	resp1 := h.postIdem(t, "/v1/storage-pools", bodyA, adminToken, key)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", resp1.StatusCode)
	}
	resp2 := h.postIdem(t, "/v1/storage-pools", bodyB, adminToken, key)
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("mismatch status = %d, want 409", resp2.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp2, &b)
	if b.Error.Code != response.CodeIdempotencyMismatch {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeIdempotencyMismatch)
	}
}

// seedVMDiskOnPool inserts a vms row plus a single vm_disks row
// referencing poolID. Mirrors seedVMNicOnNetwork in
// networks_e2e_test.go.
func seedVMDiskOnPool(t *testing.T, ctx context.Context, s *store.Store, ownerID, poolID uuid.UUID) uuid.UUID {
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
	const insDisk = `
		insert into vm_disks
		  (id, vm_id, storage_pool_id, device_order, source_kind, size_gib)
		values
		  ($1, $2, $3, 0, 'blank', 1)`
	if _, err := s.Pool().Exec(ctx, insDisk, uuid.New(), vmID, poolID); err != nil {
		t.Fatalf("insert vm_disks: %v", err)
	}
	return vmID
}

// ---- scan handler ----

// seedNodeForScan creates a node row with the supplied status. Used by
// the scan-handler tests that want a Ready node (happy path) or one in
// a 409-triggering state.
func seedNodeForScan(t *testing.T, ctx context.Context, s *store.Store, status store.NodeStatus) uuid.UUID {
	t.Helper()
	id := uuid.New()
	name := "scan-node-" + uuid.NewString()[:8]
	if _, err := s.Queries().CreateNode(ctx, store.CreateNodeParams{
		ID:                      id,
		Name:                    name,
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://" + name + ".otherix.local:9443",
		MigrationHost:           "10.0.0.10",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  status,
	}); err != nil {
		t.Fatalf("seed node (status=%v): %v", status, err)
	}
	return id
}

// seedPoolForScan creates a storage pool on nodeID and returns its id.
func seedPoolForScan(t *testing.T, ctx context.Context, s *store.Store, nodeID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	name := "scan-pool-" + uuid.NewString()[:8]
	if _, err := s.Queries().CreateStoragePool(ctx, store.CreateStoragePoolParams{
		ID:     id,
		NodeID: nodeID,
		Name:   name,
		Type:   "local_dir",
		Path:   "/opt/otherix/pools/" + name,
		Config: []byte(`{}`),
	}); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	return id
}

type asyncTaskAcceptedE2E struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
	Links  struct {
		Self string `json:"self"`
	} `json:"links"`
}

func TestE2E_StoragePoolsScan_HappyAccepted(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	nodeID := seedNodeForScan(t, ctx, h.store, store.NodeStatusReady)
	poolID := seedPoolForScan(t, ctx, h.store, nodeID)

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/storage-pools/"+poolID.String()+"/scan", map[string]any{}, adminToken)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	var body asyncTaskAcceptedE2E
	decodeJSON(t, resp, &body)
	if body.TaskID == "" {
		t.Error("task_id is empty")
	}
	taskID, err := uuid.Parse(body.TaskID)
	if err != nil {
		t.Fatalf("task_id %q is not a uuid: %v", body.TaskID, err)
	}
	if body.Status != "pending" {
		t.Errorf("status = %q, want pending", body.Status)
	}
	if body.Links.Self != "/v1/tasks/"+body.TaskID {
		t.Errorf("links.self = %q, want /v1/tasks/%s", body.Links.Self, body.TaskID)
	}

	// Task row exists with the expected shape.
	row, err := h.store.Queries().GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if row.Status != store.TaskStatusPending {
		t.Errorf("task.Status = %q, want pending", row.Status)
	}
	if row.Type != "storage_pool.scan" {
		t.Errorf("task.Type = %q, want storage_pool.scan", row.Type)
	}
	if row.ResourceType != "storage_pool" {
		t.Errorf("task.ResourceType = %q, want storage_pool", row.ResourceType)
	}
	if row.ResourceID == nil || *row.ResourceID != poolID {
		t.Errorf("task.ResourceID = %v, want %v", row.ResourceID, poolID)
	}
	if row.RiverJobID == nil || *row.RiverJobID == 0 {
		t.Errorf("task.RiverJobID = %v, want non-zero (stamped via UpdateTaskRiverJobID)", row.RiverJobID)
	}
}

func TestE2E_StoragePoolsScan_NodeUnreachable409(t *testing.T) {
	testStoragePoolsScanNodeStatus409(t, store.NodeStatusUnreachable)
}

func TestE2E_StoragePoolsScan_NodeGone409(t *testing.T) {
	testStoragePoolsScanNodeStatus409(t, store.NodeStatusGone)
}

func TestE2E_StoragePoolsScan_NodeDraining409(t *testing.T) {
	testStoragePoolsScanNodeStatus409(t, store.NodeStatusDraining)
}

func testStoragePoolsScanNodeStatus409(t *testing.T, status store.NodeStatus) {
	t.Helper()
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	nodeID := seedNodeForScan(t, ctx, h.store, status)
	poolID := seedPoolForScan(t, ctx, h.store, nodeID)

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/storage-pools/"+poolID.String()+"/scan", map[string]any{}, adminToken)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeConflict {
		t.Errorf("error.code = %q, want %q", b.Error.Code, response.CodeConflict)
	}
	if got, _ := b.Error.Details["current_status"].(string); got != string(status) {
		t.Errorf("details.current_status = %v, want %q", b.Error.Details["current_status"], status)
	}
}

func TestE2E_StoragePoolsScan_PoolNotFound404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/storage-pools/"+uuid.New().String()+"/scan", map[string]any{}, adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeNotFound {
		t.Errorf("error.code = %q, want %q", b.Error.Code, response.CodeNotFound)
	}
}

func TestE2E_StoragePoolsScan_PoolSoftDeleted404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	nodeID := seedNodeForScan(t, ctx, h.store, store.NodeStatusReady)
	poolID := seedPoolForScan(t, ctx, h.store, nodeID)
	if err := h.store.Queries().SoftDeleteStoragePool(ctx, poolID); err != nil {
		t.Fatalf("SoftDeleteStoragePool: %v", err)
	}

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/storage-pools/"+poolID.String()+"/scan", map[string]any{}, adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (soft-deleted pool invisible)", resp.StatusCode)
	}
}

// TestE2E_StoragePoolsScan_UnknownNameSurfacesAs404 — the path
// identifier is polymorphic: a non-UUID, non-existent value is
// treated as a name lookup and returns 404 (no leak) rather than a
// 400 validation envelope.
func TestE2E_StoragePoolsScan_UnknownNameSurfacesAs404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/storage-pools/not-a-pool/scan", map[string]any{}, adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_StoragePoolsScan_DeveloperForbidden403(t *testing.T) {
	testStoragePoolsScanForbidden(t, auth.RoleDeveloper)
}

func TestE2E_StoragePoolsScan_ViewerForbidden403(t *testing.T) {
	testStoragePoolsScanForbidden(t, auth.RoleViewer)
}

func testStoragePoolsScanForbidden(t *testing.T, role auth.Role) {
	t.Helper()
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	nodeID := seedNodeForScan(t, ctx, h.store, store.NodeStatusReady)
	poolID := seedPoolForScan(t, ctx, h.store, nodeID)

	token := loginAs(t, h, role)
	resp := h.post(t, "/v1/storage-pools/"+poolID.String()+"/scan", map[string]any{}, token)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status (%s) = %d, want 403", role, resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodePermissionDenied {
		t.Errorf("error.code = %q, want %q", b.Error.Code, response.CodePermissionDenied)
	}
}

func TestE2E_StoragePoolsScan_IdempotencyReplay(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	nodeID := seedNodeForScan(t, ctx, h.store, store.NodeStatusReady)
	poolID := seedPoolForScan(t, ctx, h.store, nodeID)

	adminToken := loginAs(t, h, auth.RoleAdmin)
	idemKey := "scan-idem-" + uuid.NewString()

	first := h.postWithHeaders(t, "/v1/storage-pools/"+poolID.String()+"/scan",
		map[string]any{}, adminToken,
		map[string]string{middleware.HeaderIdempotencyKey: idemKey})
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", first.StatusCode)
	}
	firstBody, err := io.ReadAll(first.Body)
	if err != nil {
		t.Fatalf("read first body: %v", err)
	}
	first.Body.Close()

	second := h.postWithHeaders(t, "/v1/storage-pools/"+poolID.String()+"/scan",
		map[string]any{}, adminToken,
		map[string]string{middleware.HeaderIdempotencyKey: idemKey})
	if second.StatusCode != http.StatusAccepted {
		t.Fatalf("second status = %d, want 202 (cached replay)", second.StatusCode)
	}
	secondBody, err := io.ReadAll(second.Body)
	if err != nil {
		t.Fatalf("read second body: %v", err)
	}
	second.Body.Close()

	if !bytes.Equal(firstBody, secondBody) {
		t.Errorf("replayed body differs:\n  first:  %s\n  second: %s", firstBody, secondBody)
	}

	// Cached body carries the same task_id; only one task row exists.
	var firstParsed asyncTaskAcceptedE2E
	if err := json.Unmarshal(firstBody, &firstParsed); err != nil {
		t.Fatalf("unmarshal first: %v", err)
	}
	var secondParsed asyncTaskAcceptedE2E
	if err := json.Unmarshal(secondBody, &secondParsed); err != nil {
		t.Fatalf("unmarshal second: %v", err)
	}
	if firstParsed.TaskID != secondParsed.TaskID {
		t.Errorf("task_id differs: first=%q second=%q (replay must be byte-identical)",
			firstParsed.TaskID, secondParsed.TaskID)
	}
}

func TestE2E_StoragePoolsScan_IdempotencyMismatch(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	nodeID := seedNodeForScan(t, ctx, h.store, store.NodeStatusReady)
	poolID := seedPoolForScan(t, ctx, h.store, nodeID)

	adminToken := loginAs(t, h, auth.RoleAdmin)
	idemKey := "scan-mismatch-" + uuid.NewString()

	first := h.postWithHeaders(t, "/v1/storage-pools/"+poolID.String()+"/scan",
		map[string]any{}, adminToken,
		map[string]string{middleware.HeaderIdempotencyKey: idemKey})
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", first.StatusCode)
	}
	first.Body.Close()

	// Same key, different body → 409 idempotency_key_mismatch from the
	// middleware before the handler is re-entered.
	second := h.postWithHeaders(t, "/v1/storage-pools/"+poolID.String()+"/scan",
		map[string]any{"reserved": "future"}, adminToken,
		map[string]string{middleware.HeaderIdempotencyKey: idemKey})
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second status = %d, want 409 (mismatch)", second.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, second, &b)
	if b.Error.Code != response.CodeIdempotencyMismatch {
		t.Errorf("error.code = %q, want %q", b.Error.Code, response.CodeIdempotencyMismatch)
	}
}
