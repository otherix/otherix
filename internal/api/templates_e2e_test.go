// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// uniqueTemplateName produces a name unique to this test run so the
// global uq_templates_name doesn't bleed across cases sharing the
// container.
func uniqueTemplateName(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

// templateBody returns a minimal valid POST /v1/templates payload.
func templateBody(name string) map[string]any {
	return map[string]any{
		"name":                  name,
		"architecture":          "amd64",
		"os_family":             "linux",
		"image_url":             "https://images.example.test/img.qcow2",
		"image_checksum_sha256": strings.Repeat("0a", 32),
	}
}

// loginAsReturningUserID seeds a fresh user with role and returns the
// caller's id along with the access token. Useful when tests need to
// assert on `owner_id`.
func loginAsReturningUserID(t *testing.T, h *e2eHarness, role auth.Role) (uuid.UUID, string) {
	t.Helper()
	id, email, pw := seedUserWithRole(t, context.Background(), h.store, role)
	resp := h.post(t, "/v1/auth/login", map[string]string{"email": email, "password": pw}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login(%s) status = %d, want 200", role, resp.StatusCode)
	}
	var login struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, resp, &login)
	return id, login.AccessToken
}

// createTemplate posts the body as the bearer and returns the parsed
// id. Fails the test on non-201.
// createTemplate seeds a template via the API and returns its
// operator-facing name (the body's `name` field). `/v1/templates/{name}`
// is name-only — call sites that build path URLs use the returned
// value directly. Tests that need the UUID for SQL-direct fixtures
// resolve it via lookupTemplateID below.
func createTemplate(t *testing.T, h *e2eHarness, body map[string]any, bearer string) string {
	t.Helper()
	resp := h.post(t, "/v1/templates", body, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create template status = %d, want 201", resp.StatusCode)
	}
	name, _ := body["name"].(string)
	if name == "" {
		t.Fatal("createTemplate: body missing required \"name\" field")
	}
	return name
}

// lookupTemplateID resolves a template name to its row UUID for
// SQL-direct fixtures that touch the DB outside the public surface.
func lookupTemplateID(t *testing.T, h *e2eHarness, name string) string {
	t.Helper()
	row, err := h.store.Queries().GetTemplateByName(context.Background(), name)
	if err != nil {
		t.Fatalf("lookup template by name %q: %v", name, err)
	}
	return row.ID.String()
}

// publishTemplate flips visibility to public via the dedicated
// endpoint. Used to set up cross-role visibility scenarios.
func publishTemplate(t *testing.T, h *e2eHarness, id, adminBearer string) {
	t.Helper()
	resp := h.post(t, "/v1/templates/"+id+"/set-visibility",
		map[string]string{"visibility": "public"}, adminBearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("publish status = %d, want 200", resp.StatusCode)
	}
}

func TestE2E_Templates_CreateRBAC(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	cases := []struct {
		role   auth.Role
		status int
	}{
		{auth.RoleAdmin, http.StatusCreated},
		{auth.RoleOperator, http.StatusCreated},
		{auth.RoleDeveloper, http.StatusCreated},
		{auth.RoleViewer, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(string(tc.role), func(t *testing.T) {
			tok := loginAs(t, h, tc.role)
			resp := h.post(t, "/v1/templates", templateBody(uniqueTemplateName("rbac-"+string(tc.role))), tok)
			if resp.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
}

func TestE2E_Templates_CreateHappyPath(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	devID, devTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)
	name := uniqueTemplateName("happy")

	resp := h.post(t, "/v1/templates", templateBody(name), devTok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var v struct {
		ID                  string `json:"id"`
		OwnerID             string `json:"owner_id"`
		Name                string `json:"name"`
		Visibility          string `json:"visibility"`
		Architecture        string `json:"architecture"`
		OSFamily            string `json:"os_family"`
		ImageURL            string `json:"image_url"`
		ImageChecksumSHA256 string `json:"image_checksum_sha256"`
		ImageFormat         string `json:"image_format"`
		FirmwareType        string `json:"firmware_type"`
		DefaultCPUCores     int    `json:"default_cpu_cores"`
		DefaultMemoryMiB    int    `json:"default_memory_mib"`
		DefaultDiskGiB      int    `json:"default_disk_gib"`
		CloudInitSupported  bool   `json:"cloud_init_supported"`
	}
	decodeJSON(t, resp, &v)
	if v.OwnerID != devID.String() {
		t.Errorf("owner_id = %q, want caller's %q", v.OwnerID, devID)
	}
	if v.Visibility != "private" {
		t.Errorf("visibility = %q, want private", v.Visibility)
	}
	if v.ImageFormat != "qcow2" || v.FirmwareType != "uefi" {
		t.Errorf("defaults: image_format=%q firmware_type=%q", v.ImageFormat, v.FirmwareType)
	}
	if v.DefaultCPUCores != 2 || v.DefaultMemoryMiB != 2048 || v.DefaultDiskGiB != 20 {
		t.Errorf("sizing defaults: cpu=%d mem=%d disk=%d", v.DefaultCPUCores, v.DefaultMemoryMiB, v.DefaultDiskGiB)
	}
	if !v.CloudInitSupported {
		t.Error("cloud_init_supported default = false, want true")
	}
	if v.Name != name {
		t.Errorf("name = %q, want %q", v.Name, name)
	}
}

func TestE2E_Templates_CreateRejectsForbiddenFields(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	tok := loginAs(t, h, auth.RoleDeveloper)
	for _, key := range []string{"visibility", "owner_id", "id", "created_at", "updated_at", "deleted_at"} {
		t.Run(key, func(t *testing.T) {
			body := templateBody(uniqueTemplateName("forbid-" + key))
			body[key] = "x"
			resp := h.post(t, "/v1/templates", body, tok)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var b response.ErrorBody
			decodeJSON(t, resp, &b)
			fields, _ := b.Error.Details["forbidden_fields"].([]any)
			seen := false
			for _, f := range fields {
				if s, _ := f.(string); s == key {
					seen = true
				}
			}
			if !seen {
				t.Errorf("forbidden_fields = %v, missing %q", fields, key)
			}
		})
	}
}

func TestE2E_Templates_CreateValidation(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	tok := loginAs(t, h, auth.RoleDeveloper)
	cases := []struct {
		label   string
		mutator func(map[string]any)
	}{
		{"empty name", func(b map[string]any) { b["name"] = "" }},
		{"bad arch", func(b map[string]any) { b["architecture"] = "riscv64" }},
		{"bad os_family", func(b map[string]any) { b["os_family"] = "macos" }},
		{"bad checksum length", func(b map[string]any) { b["image_checksum_sha256"] = "abc" }},
		{"uppercase checksum", func(b map[string]any) { b["image_checksum_sha256"] = strings.ToUpper(strings.Repeat("0a", 32)) }},
		{"file URL", func(b map[string]any) { b["image_url"] = "file:///srv/img.qcow2" }},
		{"bad image_format", func(b map[string]any) { b["image_format"] = "vmdk" }},
		{"cpu out of range", func(b map[string]any) { b["default_cpu_cores"] = 0 }},
		{"image_size_bytes negative", func(b map[string]any) { b["image_size_bytes"] = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			body := templateBody(uniqueTemplateName("val"))
			tc.mutator(body)
			resp := h.post(t, "/v1/templates", body, tok)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestE2E_Templates_CreateNameConflictNeutral(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	tokA := loginAs(t, h, auth.RoleDeveloper)
	tokB := loginAs(t, h, auth.RoleDeveloper)

	name := uniqueTemplateName("collide")
	if got := createTemplate(t, h, templateBody(name), tokA); got != name {
		t.Fatalf("seed first: got %q, want %q", got, name)
	}

	resp := h.post(t, "/v1/templates", templateBody(name), tokB)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeConflict {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeConflict)
	}
	// Message is intentionally neutral — no owner leak.
	if strings.Contains(strings.ToLower(b.Error.Message), "owner") {
		t.Errorf("message leaks owner: %q", b.Error.Message)
	}
}

func TestE2E_Templates_GetVisibility(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminTok := loginAs(t, h, auth.RoleAdmin)
	devID, devTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)
	otherDevTok := loginAs(t, h, auth.RoleDeveloper)
	viewerTok := loginAs(t, h, auth.RoleViewer)

	ownPriv := createTemplate(t, h, templateBody(uniqueTemplateName("own-priv")), devTok)
	ownPub := createTemplate(t, h, templateBody(uniqueTemplateName("own-pub")), devTok)
	publishTemplate(t, h, ownPub, adminTok)
	_ = devID

	// Owner sees both their private and public.
	for _, id := range []string{ownPriv, ownPub} {
		if r := h.get(t, "/v1/templates/"+id, devTok); r.StatusCode != http.StatusOK {
			t.Errorf("owner Get %s status = %d, want 200", id, r.StatusCode)
		}
	}

	// Other developer sees the public, 404 on owner's private.
	if r := h.get(t, "/v1/templates/"+ownPub, otherDevTok); r.StatusCode != http.StatusOK {
		t.Errorf("other-dev Get public status = %d, want 200", r.StatusCode)
	}
	if r := h.get(t, "/v1/templates/"+ownPriv, otherDevTok); r.StatusCode != http.StatusNotFound {
		t.Errorf("other-dev Get private status = %d, want 404 (no leak)", r.StatusCode)
	}

	// Viewer sees public, 404 on private (no template:read at all).
	if r := h.get(t, "/v1/templates/"+ownPub, viewerTok); r.StatusCode != http.StatusOK {
		t.Errorf("viewer Get public status = %d, want 200", r.StatusCode)
	}
	if r := h.get(t, "/v1/templates/"+ownPriv, viewerTok); r.StatusCode != http.StatusNotFound {
		t.Errorf("viewer Get private status = %d, want 404", r.StatusCode)
	}

	// Admin sees both.
	for _, id := range []string{ownPriv, ownPub} {
		if r := h.get(t, "/v1/templates/"+id, adminTok); r.StatusCode != http.StatusOK {
			t.Errorf("admin Get %s status = %d, want 200", id, r.StatusCode)
		}
	}

	// Unknown id → 404 for everyone.
	if r := h.get(t, "/v1/templates/no-such-template-name", adminTok); r.StatusCode != http.StatusNotFound {
		t.Errorf("Get unknown id status = %d, want 404", r.StatusCode)
	}
}

func TestE2E_Templates_ListVisibilityClamp(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminTok := loginAs(t, h, auth.RoleAdmin)
	devID, devTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)
	_ = devID
	otherDevTok := loginAs(t, h, auth.RoleDeveloper)
	viewerTok := loginAs(t, h, auth.RoleViewer)

	mine := createTemplate(t, h, templateBody(uniqueTemplateName("mine-priv")), devTok)
	pubID := createTemplate(t, h, templateBody(uniqueTemplateName("pub")), devTok)
	publishTemplate(t, h, pubID, adminTok)

	// Helper: page through /v1/templates and look for a name.
	// createTemplate returns the operator-facing name (used as the
	// path identifier elsewhere in this file), so this match keys off
	// the response's `name` field rather than `id`.
	listHas := func(token, name string, query string) bool {
		t.Helper()
		path := "/v1/templates?limit=200" + query
		resp := h.get(t, path, token)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list status = %d, want 200 (query=%q)", resp.StatusCode, query)
		}
		var lr struct {
			Data []struct{ Name string }
		}
		decodeJSON(t, resp, &lr)
		for _, r := range lr.Data {
			if r.Name == name {
				return true
			}
		}
		return false
	}

	// Owner sees own private and public.
	if !listHas(devTok, mine, "") || !listHas(devTok, pubID, "") {
		t.Error("owner missing own templates in list")
	}
	// Other developer: public yes, private no.
	if !listHas(otherDevTok, pubID, "") {
		t.Error("other-dev missing public in list")
	}
	if listHas(otherDevTok, mine, "") {
		t.Error("other-dev sees private template (leak)")
	}
	// Viewer: public yes, private no — even with ?visibility=private filter.
	if !listHas(viewerTok, pubID, "") {
		t.Error("viewer missing public in list")
	}
	if listHas(viewerTok, mine, "") {
		t.Error("viewer sees private template (leak)")
	}
	if listHas(viewerTok, mine, "&visibility=private") {
		t.Error("viewer with ?visibility=private leaked private template")
	}
	// Admin sees both.
	if !listHas(adminTok, mine, "") || !listHas(adminTok, pubID, "") {
		t.Error("admin missing templates in list")
	}
}

func TestE2E_Templates_ListOwnerFilterClamp(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminTok := loginAs(t, h, auth.RoleAdmin)
	aliceID, aliceTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)
	bobTok := loginAs(t, h, auth.RoleDeveloper)

	alicePriv := createTemplate(t, h, templateBody(uniqueTemplateName("alice-priv")), aliceTok)
	alicePub := createTemplate(t, h, templateBody(uniqueTemplateName("alice-pub")), aliceTok)
	publishTemplate(t, h, alicePub, adminTok)

	resp := h.get(t, "/v1/templates?owner_id="+aliceID.String(), bobTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var lr struct {
		Data []struct {
			Name       string
			Visibility string
		}
	}
	decodeJSON(t, resp, &lr)
	gotPub, gotPriv := false, false
	for _, r := range lr.Data {
		if r.Name == alicePub {
			gotPub = true
		}
		if r.Name == alicePriv {
			gotPriv = true
		}
	}
	if !gotPub {
		t.Error("bob with ?owner_id=alice missing alice's public template")
	}
	if gotPriv {
		t.Error("bob with ?owner_id=alice leaked alice's private template")
	}
}

func TestE2E_Templates_UpdateOwnAndCrossUser(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	devTok := loginAs(t, h, auth.RoleDeveloper)
	otherTok := loginAs(t, h, auth.RoleDeveloper)
	adminTok := loginAs(t, h, auth.RoleAdmin)

	id := createTemplate(t, h, templateBody(uniqueTemplateName("upd")), devTok)

	// Own dev: 200, fields stick. The path keys on name -
	// renaming the template via PATCH would invalidate the path for the
	// follow-up PATCHes, so the rename is exercised under a separate
	// test rather than chained here.
	resp := h.patch(t, "/v1/templates/"+id, map[string]any{
		"description":        "patched desc",
		"default_cpu_cores":  4,
		"default_memory_mib": 4096,
	}, devTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("own dev PATCH status = %d, want 200", resp.StatusCode)
	}
	var v struct {
		Description     string `json:"description"`
		DefaultCPUCores int    `json:"default_cpu_cores"`
	}
	decodeJSON(t, resp, &v)
	if v.Description != "patched desc" || v.DefaultCPUCores != 4 {
		t.Errorf("patch did not stick: %+v", v)
	}

	// Other developer: 404 (no leak), not 403.
	resp = h.patch(t, "/v1/templates/"+id, map[string]any{"description": "stolen"}, otherTok, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-user PATCH status = %d, want 404", resp.StatusCode)
	}

	// Admin can patch anyone's template.
	resp = h.patch(t, "/v1/templates/"+id, map[string]any{"description": "admin patch"}, adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("admin PATCH status = %d, want 200", resp.StatusCode)
	}
}

func TestE2E_Templates_UpdateRejectsForbiddenFields(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	tok := loginAs(t, h, auth.RoleDeveloper)
	id := createTemplate(t, h, templateBody(uniqueTemplateName("upd-forbid")), tok)

	keys := []string{
		"architecture", "os_family", "owner_id",
		"image_url", "image_checksum_sha256", "image_format", "image_size_bytes",
		"visibility", "id", "created_at", "updated_at", "deleted_at",
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			resp := h.patch(t, "/v1/templates/"+id, map[string]any{key: "x"}, tok, "")
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var b response.ErrorBody
			decodeJSON(t, resp, &b)
			seen := false
			fields, _ := b.Error.Details["forbidden_fields"].([]any)
			for _, f := range fields {
				if s, _ := f.(string); s == key {
					seen = true
				}
			}
			if !seen {
				t.Errorf("forbidden_fields = %v, missing %q", fields, key)
			}
		})
	}
}

func TestE2E_Templates_DeleteOwnAndCrossUser(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	devTok := loginAs(t, h, auth.RoleDeveloper)
	otherTok := loginAs(t, h, auth.RoleDeveloper)

	id := createTemplate(t, h, templateBody(uniqueTemplateName("del")), devTok)

	// Cross-user developer: 404.
	if r := h.delete(t, "/v1/templates/"+id, otherTok); r.StatusCode != http.StatusNotFound {
		t.Errorf("cross-user delete status = %d, want 404", r.StatusCode)
	}
	// Own delete: 204.
	if r := h.delete(t, "/v1/templates/"+id, devTok); r.StatusCode != http.StatusNoContent {
		t.Errorf("own delete status = %d, want 204", r.StatusCode)
	}
	// Re-delete: 404 (soft-deleted, invisible).
	if r := h.delete(t, "/v1/templates/"+id, devTok); r.StatusCode != http.StatusNotFound {
		t.Errorf("repeat delete status = %d, want 404", r.StatusCode)
	}
}

func TestE2E_Templates_DeleteBlockedByActiveVM(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, devTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)

	tplID := createTemplate(t, h, templateBody(uniqueTemplateName("blocked")), devTok)
	tplUUID := uuid.MustParse(lookupTemplateID(t, h, tplID))
	seedActiveVMOnTemplate(t, ctx, h.store, devID, tplUUID)

	resp := h.delete(t, "/v1/templates/"+tplID, devTok)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	// Lock-in: the vm-only branch must keep emitting the generic
	// `conflict` code. The dedicated `template_in_use` code only
	// fires when storage_images > 0.
	if b.Error.Code != response.CodeConflict {
		t.Errorf("code = %q, want %q (vm-only branch must stay generic)",
			b.Error.Code, response.CodeConflict)
	}
	br, _ := b.Error.Details["blocking_resources"].(map[string]any)
	if br == nil {
		t.Fatalf("missing blocking_resources: %+v", b.Error.Details)
	}
	if vms, _ := br["vms"].(float64); vms < 1 {
		t.Errorf("blocking_resources.vms = %v, want >=1", br["vms"])
	}
	if _, hasImages := br["storage_images"]; hasImages {
		t.Errorf("blocking_resources.storage_images present = %v, want absent", br["storage_images"])
	}
}

// TestE2E_Templates_DeleteBlockedByStorageImages locks in the rule
// that `CountStorageImagesByTemplate > 0` refuses the delete with the
// dedicated `template_in_use` code and a `storage_images` count in
// the blocking-resources map. No active VMs in this scenario.
func TestE2E_Templates_DeleteBlockedByStorageImages(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, devTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)

	tplID := createTemplate(t, h, templateBody(uniqueTemplateName("img-block")), devTok)
	tplUUID := uuid.MustParse(lookupTemplateID(t, h, tplID))

	nodeID := seedNodeForE2E(t, ctx, h.store)
	poolStr := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("tpl-img"))
	poolID := uuid.MustParse(poolStr)
	_ = devID // template ownership already established by createTemplate
	seedStorageImage(t, ctx, h.store, tplUUID, poolID, imageSHA256E2E(0xe5), 4096, "qcow2")

	resp := h.delete(t, "/v1/templates/"+tplID, devTok)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeResourceInUse {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeResourceInUse)
	}
	if got, _ := b.Error.Details["kind"].(string); got != "template" {
		t.Errorf("details.kind = %v, want \"template\"", b.Error.Details["kind"])
	}
	br, _ := b.Error.Details["blocking_resources"].(map[string]any)
	if br == nil {
		t.Fatalf("missing blocking_resources: %+v", b.Error.Details)
	}
	if got, _ := br["storage_images"].(float64); got < 1 {
		t.Errorf("blocking_resources.storage_images = %v, want >=1", br["storage_images"])
	}
	if _, hasVMs := br["vms"]; hasVMs {
		t.Errorf("blocking_resources.vms present = %v, want absent (no VMs in this scenario)", br["vms"])
	}
}

// TestE2E_Templates_DeleteBlockedMultiResource asserts the multi-resource
// envelope when a template is blocked by BOTH active VMs and
// storage_images. Spec §5.5 + plan Step 6: stacked refusal carries the
// dedicated `template_in_use` code and includes both keys in the
// blocking-resources map.
func TestE2E_Templates_DeleteBlockedMultiResource(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, devTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)

	tplID := createTemplate(t, h, templateBody(uniqueTemplateName("multi-block")), devTok)
	tplUUID := uuid.MustParse(lookupTemplateID(t, h, tplID))

	seedActiveVMOnTemplate(t, ctx, h.store, devID, tplUUID)
	nodeID := seedNodeForE2E(t, ctx, h.store)
	poolStr := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("multi"))
	poolID := uuid.MustParse(poolStr)
	seedStorageImage(t, ctx, h.store, tplUUID, poolID, imageSHA256E2E(0xf6), 8192, "qcow2")

	resp := h.delete(t, "/v1/templates/"+tplID, devTok)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeResourceInUse {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeResourceInUse)
	}
	if got, _ := b.Error.Details["kind"].(string); got != "template" {
		t.Errorf("details.kind = %v, want \"template\"", b.Error.Details["kind"])
	}
	br, _ := b.Error.Details["blocking_resources"].(map[string]any)
	if br == nil {
		t.Fatalf("missing blocking_resources: %+v", b.Error.Details)
	}
	if got, _ := br["storage_images"].(float64); got < 1 {
		t.Errorf("blocking_resources.storage_images = %v, want >=1", br["storage_images"])
	}
	if got, _ := br["vms"].(float64); got < 1 {
		t.Errorf("blocking_resources.vms = %v, want >=1", br["vms"])
	}
}

func TestE2E_Templates_DeleteSoftDeletedVMsDontBlock(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	devID, devTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)

	tplID := createTemplate(t, h, templateBody(uniqueTemplateName("soft-del-vm")), devTok)
	tplUUID := uuid.MustParse(lookupTemplateID(t, h, tplID))
	seedSoftDeletedVMOnTemplate(t, ctx, h.store, devID, tplUUID)

	if r := h.delete(t, "/v1/templates/"+tplID, devTok); r.StatusCode != http.StatusNoContent {
		t.Errorf("delete status = %d, want 204 (soft-deleted VMs must not block)", r.StatusCode)
	}
}

func TestE2E_Templates_SetVisibility(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminTok := loginAs(t, h, auth.RoleAdmin)
	opTok := loginAs(t, h, auth.RoleOperator)
	devID, devTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)
	viewerTok := loginAs(t, h, auth.RoleViewer)

	id := createTemplate(t, h, templateBody(uniqueTemplateName("vis")), devTok)

	// Developer / viewer → 403.
	for _, tok := range []string{devTok, viewerTok} {
		resp := h.post(t, "/v1/templates/"+id+"/set-visibility",
			map[string]string{"visibility": "public"}, tok)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("non-admin setVisibility status = %d, want 403", resp.StatusCode)
		}
	}

	// Operator publishes — owner stays the developer.
	resp := h.post(t, "/v1/templates/"+id+"/set-visibility",
		map[string]string{"visibility": "public"}, opTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("operator publish status = %d, want 200", resp.StatusCode)
	}
	var v struct {
		Visibility string `json:"visibility"`
		OwnerID    string `json:"owner_id"`
		UpdatedAt  string `json:"updated_at"`
	}
	decodeJSON(t, resp, &v)
	if v.Visibility != "public" {
		t.Errorf("visibility = %q, want public", v.Visibility)
	}
	if v.OwnerID != devID.String() {
		t.Errorf("owner_id moved on publish: got %q, want %q", v.OwnerID, devID)
	}
	publishedUpdatedAt := v.UpdatedAt

	// Same-value short-circuit: re-set to public; updated_at must NOT advance.
	resp = h.post(t, "/v1/templates/"+id+"/set-visibility",
		map[string]string{"visibility": "public"}, adminTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("same-value status = %d, want 200", resp.StatusCode)
	}
	decodeJSON(t, resp, &v)
	if v.UpdatedAt != publishedUpdatedAt {
		t.Errorf("same-value setVisibility moved updated_at: %q → %q", publishedUpdatedAt, v.UpdatedAt)
	}

	// Round-trip back to private.
	resp = h.post(t, "/v1/templates/"+id+"/set-visibility",
		map[string]string{"visibility": "private"}, adminTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin to-private status = %d, want 200", resp.StatusCode)
	}
	decodeJSON(t, resp, &v)
	if v.Visibility != "private" {
		t.Errorf("visibility = %q, want private", v.Visibility)
	}
}

func TestE2E_Templates_SetVisibilityNotFound(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminTok := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/templates/no-such-template-name"+"/set-visibility",
		map[string]string{"visibility": "public"}, adminTok)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_Templates_CloneRBACAndOwnership(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminTok := loginAs(t, h, auth.RoleAdmin)
	aliceID, aliceTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)
	bobID, bobTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)
	viewerTok := loginAs(t, h, auth.RoleViewer)
	_ = aliceID

	// Source: alice's private + alice's public.
	alicePriv := createTemplate(t, h, templateBody(uniqueTemplateName("a-priv")), aliceTok)
	alicePub := createTemplate(t, h, templateBody(uniqueTemplateName("a-pub")), aliceTok)
	publishTemplate(t, h, alicePub, adminTok)

	// Bob clones alice's public — success, owner=bob, visibility=private.
	resp := h.post(t, "/v1/templates/"+alicePub+"/clone",
		map[string]any{"name": uniqueTemplateName("bob-of-pub")}, bobTok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("bob clone-of-public status = %d, want 201", resp.StatusCode)
	}
	var v struct {
		OwnerID    string `json:"owner_id"`
		Visibility string `json:"visibility"`
	}
	decodeJSON(t, resp, &v)
	if v.OwnerID != bobID.String() {
		t.Errorf("clone owner_id = %q, want bob's %q", v.OwnerID, bobID)
	}
	if v.Visibility != "private" {
		t.Errorf("clone visibility = %q, want private", v.Visibility)
	}

	// Bob clones alice's private — 404 (no leak), not 403.
	if r := h.post(t, "/v1/templates/"+alicePriv+"/clone",
		map[string]any{"name": uniqueTemplateName("bob-of-priv")}, bobTok); r.StatusCode != http.StatusNotFound {
		t.Errorf("bob clone-of-private status = %d, want 404", r.StatusCode)
	}

	// Viewer can never clone (no template:create).
	if r := h.post(t, "/v1/templates/"+alicePub+"/clone",
		map[string]any{"name": uniqueTemplateName("viewer-clone")}, viewerTok); r.StatusCode != http.StatusForbidden {
		t.Errorf("viewer clone status = %d, want 403", r.StatusCode)
	}

	// Description override + missing source → 404.
	override := "overridden description"
	resp = h.post(t, "/v1/templates/"+alicePub+"/clone",
		map[string]any{"name": uniqueTemplateName("override"), "description": override}, bobTok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("clone with description status = %d, want 201", resp.StatusCode)
	}
	var d struct{ Description string }
	decodeJSON(t, resp, &d)
	if d.Description != override {
		t.Errorf("clone description override = %q, want %q", d.Description, override)
	}

	if r := h.post(t, "/v1/templates/no-such-template-name"+"/clone",
		map[string]any{"name": uniqueTemplateName("ghost")}, bobTok); r.StatusCode != http.StatusNotFound {
		t.Errorf("clone of missing source status = %d, want 404", r.StatusCode)
	}
}

func TestE2E_Templates_CloneRejectsForbiddenFields(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	devTok := loginAs(t, h, auth.RoleDeveloper)
	id := createTemplate(t, h, templateBody(uniqueTemplateName("clone-forbid")), devTok)

	for _, key := range []string{"architecture", "default_cpu_cores", "owner_id", "visibility"} {
		t.Run(key, func(t *testing.T) {
			body := map[string]any{
				"name": uniqueTemplateName("c-" + key),
				key:    "x",
			}
			resp := h.post(t, "/v1/templates/"+id+"/clone", body, devTok)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

func TestE2E_Templates_IdempotencyReplay(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	devTok := loginAs(t, h, auth.RoleDeveloper)
	body := templateBody(uniqueTemplateName("idem"))
	key := "idem-" + uuid.NewString()

	first := h.postIdem(t, "/v1/templates", body, devTok, key)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.StatusCode)
	}
	var v1 struct{ ID string }
	decodeJSON(t, first, &v1)

	second := h.postIdem(t, "/v1/templates", body, devTok, key)
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("replay status = %d, want 201 (cached)", second.StatusCode)
	}
	var v2 struct{ ID string }
	decodeJSON(t, second, &v2)
	if v1.ID != v2.ID {
		t.Errorf("replay returned different id: %q vs %q (cache miss)", v1.ID, v2.ID)
	}

	// Mismatch on body returns 409 idempotency_key_mismatch.
	mut := templateBody(uniqueTemplateName("idem-mut"))
	third := h.postIdem(t, "/v1/templates", mut, devTok, key)
	if third.StatusCode != http.StatusConflict {
		t.Fatalf("mismatch status = %d, want 409", third.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, third, &b)
	if b.Error.Code != response.CodeIdempotencyMismatch {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeIdempotencyMismatch)
	}
}

// seedActiveVMOnTemplate inserts an active vms row referencing tplID
// so the API-level DELETE precheck has something to count.
func seedActiveVMOnTemplate(t *testing.T, ctx context.Context, s *store.Store, ownerID, tplID uuid.UUID) uuid.UUID {
	t.Helper()
	vmID := uuid.New()
	const insVM = `
		insert into vms
		  (id, owner_id, name, architecture, cpu_cores, memory_mib, machine_type, template_id)
		values
		  ($1, $2, $3, 'amd64', 1, 256, 'pc-i440fx-8.0', $4)`
	if _, err := s.Pool().Exec(ctx, insVM, vmID, ownerID, "vm-"+uuid.NewString()[:8], tplID); err != nil {
		t.Fatalf("insert vms: %v", err)
	}
	return vmID
}

// seedSoftDeletedVMOnTemplate inserts a soft-deleted vms row — the
// precheck must skip these so the template-DELETE proceeds.
func seedSoftDeletedVMOnTemplate(t *testing.T, ctx context.Context, s *store.Store, ownerID, tplID uuid.UUID) uuid.UUID {
	t.Helper()
	vmID := uuid.New()
	const insVM = `
		insert into vms
		  (id, owner_id, name, architecture, cpu_cores, memory_mib, machine_type, template_id, deleted_at)
		values
		  ($1, $2, $3, 'amd64', 1, 256, 'pc-i440fx-8.0', $4, now())`
	if _, err := s.Pool().Exec(ctx, insVM, vmID, ownerID, "vm-"+uuid.NewString()[:8], tplID); err != nil {
		t.Fatalf("insert vms: %v", err)
	}
	return vmID
}
