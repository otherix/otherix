// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// uniqueFirmwareNameE2E returns a per-call unique name so e2e tests
// running concurrently do not collide on uq_firmwares_name_arch.
func uniqueFirmwareNameE2E(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

// firmwareBody builds a minimal POST /v1/firmwares payload.
func firmwareBody(name string) map[string]any {
	return map[string]any{
		"name":         name,
		"architecture": "amd64",
		"type":         "uefi",
	}
}

// createFirmwareAsAdmin seeds a firmware via the API and returns its
// id.
func createFirmwareAsAdmin(t *testing.T, h *e2eHarness, body map[string]any) string {
	t.Helper()
	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/firmwares", body, adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed firmware: status = %d, want 201", resp.StatusCode)
	}
	var v struct {
		ID string `json:"id"`
	}
	decodeJSON(t, resp, &v)
	if v.ID == "" {
		t.Fatal("seed firmware: empty id in response")
	}
	return v.ID
}

func TestE2E_Firmwares_CreateRequiresAdmin(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.post(t, "/v1/firmwares",
				firmwareBody(uniqueFirmwareNameE2E("nope")), tok)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

func TestE2E_Firmwares_CreateHappyPath(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	name := uniqueFirmwareNameE2E("happy")
	body := firmwareBody(name)
	body["version"] = "edk2-stable202311"
	body["secure_boot"] = true

	resp := h.post(t, "/v1/firmwares", body, adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var view struct {
		ID           string  `json:"id"`
		Name         string  `json:"name"`
		Architecture string  `json:"architecture"`
		Type         string  `json:"type"`
		Version      *string `json:"version"`
		SecureBoot   bool    `json:"secure_boot"`
		IsDefault    bool    `json:"is_default"`
		CreatedAt    string  `json:"created_at"`
		UpdatedAt    string  `json:"updated_at"`
	}
	decodeJSON(t, resp, &view)

	if view.Name != name {
		t.Errorf("name = %q, want %q", view.Name, name)
	}
	if view.Architecture != "amd64" {
		t.Errorf("architecture = %q, want amd64", view.Architecture)
	}
	if view.Type != "uefi" {
		t.Errorf("type = %q, want uefi", view.Type)
	}
	if view.Version == nil || *view.Version != "edk2-stable202311" {
		t.Errorf("version = %v, want edk2-stable202311", view.Version)
	}
	if !view.SecureBoot {
		t.Error("secure_boot = false, want true")
	}
	if view.IsDefault {
		t.Error("is_default = true, want false")
	}
	if view.CreatedAt == "" || view.UpdatedAt == "" {
		t.Errorf("missing timestamps: created_at=%q updated_at=%q", view.CreatedAt, view.UpdatedAt)
	}
	if _, err := uuid.Parse(view.ID); err != nil {
		t.Errorf("id = %q, not a uuid", view.ID)
	}
}

func TestE2E_Firmwares_CreateValidation(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	cases := []struct {
		name string
		mut  func(map[string]any)
	}{
		{name: "empty name", mut: func(b map[string]any) { b["name"] = "" }},
		{name: "leading space", mut: func(b map[string]any) { b["name"] = " bad" }},
		{name: "bad architecture", mut: func(b map[string]any) { b["architecture"] = "x86_64" }},
		{name: "bad type", mut: func(b map[string]any) { b["type"] = "coreboot" }},
		{name: "version with NUL", mut: func(b map[string]any) { b["version"] = "v1\x00bad" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := firmwareBody(uniqueFirmwareNameE2E("v"))
			tc.mut(body)
			resp := h.post(t, "/v1/firmwares", body, adminToken)
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

func TestE2E_Firmwares_CreateDuplicateNameArch(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	name := uniqueFirmwareNameE2E("dup")

	resp1 := h.post(t, "/v1/firmwares", firmwareBody(name), adminToken)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", resp1.StatusCode)
	}
	resp2 := h.post(t, "/v1/firmwares", firmwareBody(name), adminToken)
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("dup status = %d, want 409", resp2.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp2, &b)
	if got, _ := b.Error.Details["field"].(string); got != "name" {
		t.Errorf("details.field = %v, want name", b.Error.Details["field"])
	}
}

func TestE2E_Firmwares_CreateRejectsSecondDefault(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)

	// Use a fresh (architecture, type) pair to isolate from any other
	// concurrently-running default-firmware fixture: arm64 + bios is
	// rarely populated by other tests.
	bodyA := firmwareBody(uniqueFirmwareNameE2E("def-a"))
	bodyA["architecture"] = "arm64"
	bodyA["type"] = "bios"
	bodyA["is_default"] = true
	respA := h.post(t, "/v1/firmwares", bodyA, adminToken)
	if respA.StatusCode != http.StatusCreated {
		t.Fatalf("first default status = %d, want 201", respA.StatusCode)
	}
	var first struct {
		ID string `json:"id"`
	}
	decodeJSON(t, respA, &first)

	bodyB := firmwareBody(uniqueFirmwareNameE2E("def-b"))
	bodyB["architecture"] = "arm64"
	bodyB["type"] = "bios"
	bodyB["is_default"] = true
	respB := h.post(t, "/v1/firmwares", bodyB, adminToken)
	if respB.StatusCode != http.StatusConflict {
		t.Fatalf("second default status = %d, want 409", respB.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, respB, &b)
	if b.Error.Code != response.CodeConflict {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeConflict)
	}
	if got, _ := b.Error.Details["code"].(string); got != "default_already_set" {
		t.Errorf("details.code = %v, want default_already_set", b.Error.Details["code"])
	}
	if got, _ := b.Error.Details["existing_firmware_id"].(string); got != first.ID {
		t.Errorf("details.existing_firmware_id = %v, want %s", b.Error.Details["existing_firmware_id"], first.ID)
	}
}

func TestE2E_Firmwares_GetAndListAllRolesCanRead(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createFirmwareAsAdmin(t, h, firmwareBody(uniqueFirmwareNameE2E("read")))

	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.get(t, "/v1/firmwares/"+id, tok)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("get status = %d, want 200", resp.StatusCode)
			}
			var view map[string]any
			decodeJSON(t, resp, &view)
			for _, k := range []string{"id", "name", "architecture", "type", "version", "secure_boot", "is_default", "created_at", "updated_at"} {
				if _, ok := view[k]; !ok {
					t.Errorf("missing %q in view", k)
				}
			}
			respList := h.get(t, "/v1/firmwares", tok)
			if respList.StatusCode != http.StatusOK {
				t.Errorf("list status = %d, want 200", respList.StatusCode)
			}
		})
	}
}

func TestE2E_Firmwares_GetUnknown404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/firmwares/"+uuid.NewString(), adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_Firmwares_ListByArchitectureAndType(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	createFirmwareAsAdmin(t, h, firmwareBody(uniqueFirmwareNameE2E("flt")))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	resp := h.get(t, "/v1/firmwares?architecture=amd64&type=uefi", adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var page struct {
		Data []map[string]any `json:"data"`
	}
	decodeJSON(t, resp, &page)
	for _, row := range page.Data {
		if row["architecture"] != "amd64" {
			t.Errorf("filter ignored: arch = %v, want amd64", row["architecture"])
		}
		if row["type"] != "uefi" {
			t.Errorf("filter ignored: type = %v, want uefi", row["type"])
		}
	}

	respBadArch := h.get(t, "/v1/firmwares?architecture=invalid", adminToken)
	if respBadArch.StatusCode != http.StatusBadRequest {
		t.Errorf("bad-arch status = %d, want 400", respBadArch.StatusCode)
	}
	respBadType := h.get(t, "/v1/firmwares?type=invalid", adminToken)
	if respBadType.StatusCode != http.StatusBadRequest {
		t.Errorf("bad-type status = %d, want 400", respBadType.StatusCode)
	}
}

func TestE2E_Firmwares_UpdateRequiresAdmin(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createFirmwareAsAdmin(t, h, firmwareBody(uniqueFirmwareNameE2E("upd-rbac")))
	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.patch(t, "/v1/firmwares/"+id,
				map[string]any{"name": "renamed"}, tok, "")
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

func TestE2E_Firmwares_UpdateMutableFields(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createFirmwareAsAdmin(t, h, firmwareBody(uniqueFirmwareNameE2E("upd")))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	newName := uniqueFirmwareNameE2E("renamed")
	body := map[string]any{
		"name":        newName,
		"version":     "edk2-stable202405",
		"secure_boot": true,
	}
	resp := h.patch(t, "/v1/firmwares/"+id, body, adminToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var view struct {
		Name       string  `json:"name"`
		Version    *string `json:"version"`
		SecureBoot bool    `json:"secure_boot"`
	}
	decodeJSON(t, resp, &view)
	if view.Name != newName {
		t.Errorf("name = %q, want %q", view.Name, newName)
	}
	if view.Version == nil || *view.Version != "edk2-stable202405" {
		t.Errorf("version = %v, want edk2-stable202405", view.Version)
	}
	if !view.SecureBoot {
		t.Error("secure_boot = false, want true")
	}
}

func TestE2E_Firmwares_UpdateClearsVersionWithNull(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	body := firmwareBody(uniqueFirmwareNameE2E("clr"))
	body["version"] = "edk2-stable202311"
	id := createFirmwareAsAdmin(t, h, body)
	adminToken := loginAs(t, h, auth.RoleAdmin)

	resp := h.patch(t, "/v1/firmwares/"+id,
		map[string]any{"version": nil}, adminToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var view struct {
		Version *string `json:"version"`
	}
	decodeJSON(t, resp, &view)
	if view.Version != nil {
		t.Errorf("version = %v, want nil after explicit null", view.Version)
	}
}

func TestE2E_Firmwares_UpdateRejectsImmutableFields(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createFirmwareAsAdmin(t, h, firmwareBody(uniqueFirmwareNameE2E("upd-imm")))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	cases := []struct {
		field string
		value any
	}{
		{field: "architecture", value: "arm64"},
		{field: "type", value: "bios"},
		{field: "id", value: uuid.NewString()},
		{field: "created_at", value: "2026-05-06T00:00:00Z"},
		{field: "updated_at", value: "2026-05-06T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			resp := h.patch(t, "/v1/firmwares/"+id,
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

func TestE2E_Firmwares_UpdatePromotionRejectsExistingDefault(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)

	// Use arm64 + uefi to keep this test isolated from
	// CreateRejectsSecondDefault (arm64 + bios) and from any concurrent
	// amd64 + uefi default fixtures.
	defBody := firmwareBody(uniqueFirmwareNameE2E("std-def"))
	defBody["architecture"] = "arm64"
	defBody["type"] = "uefi"
	defBody["is_default"] = true
	respDef := h.post(t, "/v1/firmwares", defBody, adminToken)
	if respDef.StatusCode != http.StatusCreated {
		t.Fatalf("seed default status = %d, want 201", respDef.StatusCode)
	}
	var existingDef struct {
		ID string `json:"id"`
	}
	decodeJSON(t, respDef, &existingDef)

	plainBody := firmwareBody(uniqueFirmwareNameE2E("std-plain"))
	plainBody["architecture"] = "arm64"
	plainBody["type"] = "uefi"
	plainID := createFirmwareAsAdmin(t, h, plainBody)

	resp := h.patch(t, "/v1/firmwares/"+plainID,
		map[string]any{"is_default": true}, adminToken, "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if got, _ := b.Error.Details["code"].(string); got != "default_already_set" {
		t.Errorf("details.code = %v, want default_already_set", b.Error.Details["code"])
	}
	if got, _ := b.Error.Details["existing_firmware_id"].(string); got != existingDef.ID {
		t.Errorf("details.existing_firmware_id = %v, want %s", b.Error.Details["existing_firmware_id"], existingDef.ID)
	}
}

func TestE2E_Firmwares_DeleteRequiresAdmin(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createFirmwareAsAdmin(t, h, firmwareBody(uniqueFirmwareNameE2E("del-rbac")))
	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.delete(t, "/v1/firmwares/"+id, tok)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

func TestE2E_Firmwares_DeleteHappyPath(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	id := createFirmwareAsAdmin(t, h, firmwareBody(uniqueFirmwareNameE2E("del-happy")))
	adminToken := loginAs(t, h, auth.RoleAdmin)

	resp := h.delete(t, "/v1/firmwares/"+id, adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	resp = h.get(t, "/v1/firmwares/"+id, adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("post-delete get = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_Firmwares_DeleteUnknown404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/firmwares/"+uuid.NewString(), adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_Firmwares_DeleteBlockedByVM(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	idStr := createFirmwareAsAdmin(t, h, firmwareBody(uniqueFirmwareNameE2E("del-vm")))
	fwID := uuid.MustParse(idStr)
	ownerID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	seedVMOnFirmware(t, ctx, h.store, ownerID, fwID)

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/firmwares/"+idStr, adminToken)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	br, _ := b.Error.Details["blocking_resources"].(map[string]any)
	if br == nil {
		t.Fatalf("missing blocking_resources in details: %+v", b.Error.Details)
	}
	if vms, _ := br["vms"].(float64); vms < 1 {
		t.Errorf("blocking_resources.vms = %v, want >=1", br["vms"])
	}
	if _, ok := br["templates"]; ok {
		t.Errorf("blocking_resources.templates surfaced for vm-only block: %v", br)
	}
}

func TestE2E_Firmwares_DeleteBlockedByTemplate(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	idStr := createFirmwareAsAdmin(t, h, firmwareBody(uniqueFirmwareNameE2E("del-tpl")))
	fwID := uuid.MustParse(idStr)
	ownerID, _, _ := seedUserWithRole(t, ctx, h.store, auth.RoleDeveloper)
	seedTemplateOnFirmware(t, ctx, h.store, ownerID, fwID)

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/firmwares/"+idStr, adminToken)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	br, _ := b.Error.Details["blocking_resources"].(map[string]any)
	if br == nil {
		t.Fatalf("missing blocking_resources: %+v", b.Error.Details)
	}
	if tpls, _ := br["templates"].(float64); tpls < 1 {
		t.Errorf("blocking_resources.templates = %v, want >=1", br["templates"])
	}
	if _, ok := br["vms"]; ok {
		t.Errorf("blocking_resources.vms surfaced for template-only block: %v", br)
	}
}

func TestE2E_Firmwares_IdempotencyReplay(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	body := firmwareBody(uniqueFirmwareNameE2E("idem"))
	key := "idem-" + uuid.NewString()

	resp1 := h.postIdem(t, "/v1/firmwares", body, adminToken, key)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", resp1.StatusCode)
	}
	var first map[string]any
	decodeJSON(t, resp1, &first)

	resp2 := h.postIdem(t, "/v1/firmwares", body, adminToken, key)
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("replay status = %d, want 201", resp2.StatusCode)
	}
	var second map[string]any
	decodeJSON(t, resp2, &second)
	if first["id"] != second["id"] {
		t.Errorf("ids differ on replay: %v vs %v", first["id"], second["id"])
	}
}

func TestE2E_Firmwares_IdempotencyMismatch(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	bodyA := firmwareBody(uniqueFirmwareNameE2E("idem-a"))
	bodyB := firmwareBody(uniqueFirmwareNameE2E("idem-b"))
	key := "idem-" + uuid.NewString()

	resp1 := h.postIdem(t, "/v1/firmwares", bodyA, adminToken, key)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", resp1.StatusCode)
	}
	resp2 := h.postIdem(t, "/v1/firmwares", bodyB, adminToken, key)
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("mismatch status = %d, want 409", resp2.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp2, &b)
	if b.Error.Code != response.CodeIdempotencyMismatch {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeIdempotencyMismatch)
	}
}

func TestE2E_Firmwares_NodeListEmpty(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/nodes/"+nodeID+"/firmwares", adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var page struct {
		Data []any `json:"data"`
		Meta struct {
			NextCursor *string `json:"next_cursor"`
		} `json:"meta"`
	}
	decodeJSON(t, resp, &page)
	if len(page.Data) != 0 {
		t.Errorf("node firmware data len = %d, want 0", len(page.Data))
	}
	if page.Meta.NextCursor != nil {
		t.Errorf("next_cursor = %v, want nil", page.Meta.NextCursor)
	}
}

func TestE2E_Firmwares_NodeListUnknownNode404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/nodes/no-such-node-name"+"/firmwares", adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_Firmwares_NodeListAllRolesCanRead(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	nodeID := seedNodeForE2E(t, context.Background(), h.store)
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.get(t, "/v1/nodes/"+nodeID+"/firmwares", tok)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d, want 200", resp.StatusCode)
			}
		})
	}
}

// seedVMOnFirmware inserts a vms row pinning fwID. Mirrors
// seedVMDiskOnPool from storage_pools_e2e_test.go.
func seedVMOnFirmware(t *testing.T, ctx context.Context, s *store.Store, ownerID, fwID uuid.UUID) uuid.UUID {
	t.Helper()
	vmID := uuid.New()
	const insVM = `
		insert into vms
		  (id, owner_id, name, architecture, cpu_cores, memory_mib, machine_type, firmware_id)
		values
		  ($1, $2, $3, 'amd64', 1, 256, 'pc-i440fx-8.0', $4)`
	if _, err := s.Pool().Exec(ctx, insVM, vmID, ownerID, "vm-"+uuid.NewString()[:8], fwID); err != nil {
		t.Fatalf("insert vms: %v", err)
	}
	return vmID
}

// seedTemplateOnFirmware inserts a templates row pinning fwID и
// returns both the row UUID (для FK references) и the operator-
// facing name (для name-based path constructs).
func seedTemplateOnFirmware(t *testing.T, ctx context.Context, s *store.Store, ownerID, fwID uuid.UUID) (uuid.UUID, string) {
	t.Helper()
	tplID := uuid.New()
	tplName := "tpl-" + uuid.NewString()[:8]
	const insTpl = `
		insert into templates
		  (id, owner_id, name, architecture, os_family, image_url, image_checksum_sha256, firmware_id)
		values
		  ($1, $2, $3, 'amd64', 'linux', 'https://x', '\x00', $4)`
	if _, err := s.Pool().Exec(ctx, insTpl, tplID, ownerID, tplName, fwID); err != nil {
		t.Fatalf("insert templates: %v", err)
	}
	return tplID, tplName
}
