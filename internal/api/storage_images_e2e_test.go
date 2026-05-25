// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"context"
	"encoding/hex"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// recordingImageDeleter is the test double used by the
// storage_image.delete e2e suite. It records every call so tests can
// assert agent invocation count, and returns nextErr (set per case)
// so the suite can drive the 204 / 404 / 5xx / unreachable branches
// without spinning up a real agent.
type recordingImageDeleter struct {
	calls   atomic.Int32
	nextErr atomic.Pointer[error]
}

func (r *recordingImageDeleter) DeleteImage(_ context.Context, _ string, _ string, _, _ string) error {
	r.calls.Add(1)
	if p := r.nextErr.Load(); p != nil {
		return *p
	}
	return nil
}

func (r *recordingImageDeleter) setError(err error) {
	r.nextErr.Store(&err)
}

// imageSHA256E2E returns a 64-char lowercase hex string seeded with b
// repeated 32 times — enough variability for tests, deterministic enough
// for assertion. Mirrors imageSHA256 in the store-package tests.
func imageSHA256E2E(b byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = b
	}
	return hex.EncodeToString(raw)
}

// seedFirmwareForImagesE2E inserts a flat firmware row directly through
// the store. Storage images need a firmware → template chain to satisfy
// the templates.firmware_id FK.
func seedFirmwareForImagesE2E(t *testing.T, ctx context.Context, s *store.Store) uuid.UUID {
	t.Helper()
	fw, err := s.Queries().CreateFirmware(ctx, store.CreateFirmwareParams{
		ID:           uuid.New(),
		Name:         "fw-img-" + uuid.NewString()[:8],
		Architecture: store.CpuArchAmd64,
		Type:         store.FirmwareTypeUefi,
		SecureBoot:   false,
		IsDefault:    false,
	})
	if err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}
	return fw.ID
}

// seedStorageImage inserts a storage_images row keyed on the supplied
// (template, pool) pair with the supplied checksum/size/format. Returns
// the row id. Uses CreateStorageImage so the upsert semantics are
// preserved end-to-end.
func seedStorageImage(t *testing.T, ctx context.Context, s *store.Store, templateID, poolID uuid.UUID, sha256 string, size int64, format string) uuid.UUID {
	t.Helper()
	row, err := s.Queries().CreateStorageImage(ctx, store.CreateStorageImageParams{
		ID:             uuid.New(),
		TemplateID:     templateID,
		PoolID:         poolID,
		ChecksumSha256: sha256,
		SizeBytes:      size,
		Format:         format,
	})
	if err != nil {
		t.Fatalf("CreateStorageImage: %v", err)
	}
	return row.ID
}

// imageScenario bundles the seeded prerequisite ids tests need.
type imageScenario struct {
	ownerID      uuid.UUID
	ownerToken   string
	nodeID       string
	poolID       uuid.UUID
	firmwareID   uuid.UUID
	templateID   uuid.UUID
	templateName string
}

func setupImageScenario(t *testing.T, h *e2eHarness) imageScenario {
	t.Helper()
	ctx := context.Background()

	ownerID, ownerTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)
	nodeID := seedNodeForE2E(t, ctx, h.store)
	poolIDStr := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("imgs"))
	poolID, err := uuid.Parse(poolIDStr)
	if err != nil {
		t.Fatalf("parse pool id: %v", err)
	}
	fwID := seedFirmwareForImagesE2E(t, ctx, h.store)
	tplID, tplName := seedTemplateOnFirmware(t, ctx, h.store, ownerID, fwID)
	return imageScenario{
		ownerID:      ownerID,
		ownerToken:   ownerTok,
		nodeID:       nodeID,
		poolID:       poolID,
		firmwareID:   fwID,
		templateID:   tplID,
		templateName: tplName,
	}
}

// expectedImageKeys is the canonical set of fields the public schema
// surfaces. Used by the leak-guard assertions. `template_id` /
// `pool_id` UUIDs are exposed as resolved names.
var expectedImageKeys = []string{
	"id", "template", "pool",
	"checksum_sha256", "size_bytes", "format", "imported_at",
}

func assertImageViewKeys(t *testing.T, view map[string]any) {
	t.Helper()
	for _, k := range expectedImageKeys {
		if _, ok := view[k]; !ok {
			t.Errorf("missing %q in storage image view", k)
		}
	}
	for k := range view {
		found := false
		for _, want := range expectedImageKeys {
			if k == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("unexpected key %q in storage image view (leak-guard)", k)
		}
	}
}

func TestE2E_StorageImages_ListAllRolesCanRead(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	imgID := seedStorageImage(t, context.Background(), h.store,
		sc.templateID, sc.poolID, imageSHA256E2E(0xa1), 1024, "qcow2")

	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.get(t, "/v1/storage-pools/"+sc.poolID.String()+"/images", tok)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var page struct {
				Data []map[string]any `json:"data"`
				Meta struct {
					NextCursor *string `json:"next_cursor"`
				} `json:"meta"`
			}
			decodeJSON(t, resp, &page)
			if len(page.Data) == 0 {
				t.Fatalf("data empty, want >= 1 row")
			}
			var found map[string]any
			for _, row := range page.Data {
				if row["id"] == imgID.String() {
					found = row
					break
				}
			}
			if found == nil {
				t.Fatalf("seeded image id %s not in page", imgID)
			}
			assertImageViewKeys(t, found)
			// template and pool surface as resolved names, not UUIDs.
			// We only verify the field type — the exact names are
			// scenario-dependent.
			if _, ok := found["template"].(string); !ok {
				t.Errorf("template = %v, want a name string", found["template"])
			}
			if _, ok := found["pool"].(string); !ok {
				t.Errorf("pool = %v, want a name string", found["pool"])
			}
			if got := found["format"]; got != "qcow2" {
				t.Errorf("format = %v, want qcow2", got)
			}
		})
	}
}

func TestE2E_StorageImages_GetAllRolesCanRead(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	imgID := seedStorageImage(t, context.Background(), h.store,
		sc.templateID, sc.poolID, imageSHA256E2E(0xb2), 2048, "qcow2")

	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.get(t, "/v1/storage-pools/"+sc.poolID.String()+"/images/"+imgID.String(), tok)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var view map[string]any
			decodeJSON(t, resp, &view)
			assertImageViewKeys(t, view)
			if got := view["id"]; got != imgID.String() {
				t.Errorf("id = %v, want %s", got, imgID)
			}
			if got := view["size_bytes"]; got != float64(2048) {
				// JSON decodes numeric to float64 by default — any other
				// concrete shape is a contract regression.
				t.Errorf("size_bytes = %v (%T), want 2048", got, got)
			}
		})
	}
}

func TestE2E_StorageImages_ListPaginationForward(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	ctx := context.Background()

	// Seed three rows on three distinct templates so the unique
	// (template_id, pool_id) constraint is satisfied. Order is
	// determined by imported_at DESC; since the seeds run in sequence,
	// the third inserted row appears first.
	fw := sc.firmwareID
	tplA, _ := seedTemplateOnFirmware(t, ctx, h.store, sc.ownerID, fw)
	tplB, _ := seedTemplateOnFirmware(t, ctx, h.store, sc.ownerID, fw)
	tplC, _ := seedTemplateOnFirmware(t, ctx, h.store, sc.ownerID, fw)
	idA := seedStorageImage(t, ctx, h.store, tplA, sc.poolID, imageSHA256E2E(0x01), 1, "qcow2")
	idB := seedStorageImage(t, ctx, h.store, tplB, sc.poolID, imageSHA256E2E(0x02), 2, "qcow2")
	idC := seedStorageImage(t, ctx, h.store, tplC, sc.poolID, imageSHA256E2E(0x03), 3, "qcow2")

	tok := loginAs(t, h, auth.RoleAdmin)

	// First page: limit=2 → expect the two newest (C, B).
	resp1 := h.get(t, "/v1/storage-pools/"+sc.poolID.String()+"/images?limit=2", tok)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("page1 status = %d, want 200", resp1.StatusCode)
	}
	var page1 struct {
		Data []map[string]any `json:"data"`
		Meta struct {
			NextCursor *string `json:"next_cursor"`
		} `json:"meta"`
	}
	decodeJSON(t, resp1, &page1)
	if len(page1.Data) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1.Data))
	}
	if page1.Data[0]["id"] != idC.String() || page1.Data[1]["id"] != idB.String() {
		t.Errorf("page1 ids = %v / %v, want %s / %s",
			page1.Data[0]["id"], page1.Data[1]["id"], idC, idB)
	}
	if page1.Meta.NextCursor == nil {
		t.Fatalf("page1 next_cursor is nil, want non-nil")
	}

	// Second page using the cursor. limit=2 to confirm trailing nil.
	resp2 := h.get(t,
		"/v1/storage-pools/"+sc.poolID.String()+"/images?limit=2&cursor="+*page1.Meta.NextCursor, tok)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("page2 status = %d, want 200", resp2.StatusCode)
	}
	var page2 struct {
		Data []map[string]any `json:"data"`
		Meta struct {
			NextCursor *string `json:"next_cursor"`
		} `json:"meta"`
	}
	decodeJSON(t, resp2, &page2)
	if len(page2.Data) != 1 {
		t.Fatalf("page2 len = %d, want 1", len(page2.Data))
	}
	if page2.Data[0]["id"] != idA.String() {
		t.Errorf("page2 id = %v, want %s", page2.Data[0]["id"], idA)
	}
	if page2.Meta.NextCursor != nil {
		t.Errorf("page2 next_cursor = %v, want nil", page2.Meta.NextCursor)
	}
}

func TestE2E_StorageImages_GetCrossPool404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	ctx := context.Background()
	imgID := seedStorageImage(t, ctx, h.store,
		sc.templateID, sc.poolID, imageSHA256E2E(0xc3), 512, "qcow2")

	// Second pool to address against.
	otherPoolStr := createPoolAsAdmin(t, h, sc.nodeID, uniquePoolNameE2E("other"))

	tok := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/storage-pools/"+otherPoolStr+"/images/"+imgID.String(), tok)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (cross-pool addressing)", resp.StatusCode)
	}
}

func TestE2E_StorageImages_GetUnknownID404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	tok := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t,
		"/v1/storage-pools/"+sc.poolID.String()+"/images/"+uuid.NewString(), tok)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_StorageImages_ListUnknownPool404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	tok := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/storage-pools/"+uuid.NewString()+"/images", tok)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_StorageImages_ListBadCursor400(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	tok := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t,
		"/v1/storage-pools/"+sc.poolID.String()+"/images?cursor=not-a-cursor!!!", tok)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestE2E_StorageImages_AnonRejected401(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	imgID := seedStorageImage(t, context.Background(), h.store,
		sc.templateID, sc.poolID, imageSHA256E2E(0xd4), 1, "qcow2")

	cases := []struct {
		name string
		path string
	}{
		{"list", "/v1/storage-pools/" + sc.poolID.String() + "/images"},
		{"get", "/v1/storage-pools/" + sc.poolID.String() + "/images/" + imgID.String()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.get(t, tc.path, "")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// ----- storage_image.delete e2e tests ---------------

func TestE2E_StorageImages_Delete_CountGreaterZero(t *testing.T) {
	deleter := &recordingImageDeleter{}
	h := newE2EWithImageDeleter(t, deleter)
	defer h.close()

	sc := setupImageScenario(t, h)
	ctx := context.Background()
	checksum := imageSHA256E2E(0xa1)

	// Two templates → two storage_images rows in the same pool sharing
	// the same checksum. Deleting one keeps the file alive for the
	// other; the agent must NOT be called.
	tplA, _ := seedTemplateOnFirmware(t, ctx, h.store, sc.ownerID, sc.firmwareID)
	tplB, _ := seedTemplateOnFirmware(t, ctx, h.store, sc.ownerID, sc.firmwareID)
	idA := seedStorageImage(t, ctx, h.store, tplA, sc.poolID, checksum, 1024, "qcow2")
	idB := seedStorageImage(t, ctx, h.store, tplB, sc.poolID, checksum, 1024, "qcow2")

	tok := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/storage-pools/"+sc.poolID.String()+"/images/"+idA.String(), tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := deleter.calls.Load(); got != 0 {
		t.Errorf("agent calls = %d, want 0 (count > 0 path must not invoke agent)", got)
	}

	// idA gone, idB stays.
	if _, err := h.store.Queries().GetStorageImageByID(ctx, idA); err == nil {
		t.Errorf("idA still present after delete")
	}
	if _, err := h.store.Queries().GetStorageImageByID(ctx, idB); err != nil {
		t.Errorf("idB lookup: %v (sibling was wrongly deleted)", err)
	}
}

func TestE2E_StorageImages_Delete_CountZeroAgentSuccess(t *testing.T) {
	deleter := &recordingImageDeleter{} // returns nil
	h := newE2EWithImageDeleter(t, deleter)
	defer h.close()

	sc := setupImageScenario(t, h)
	ctx := context.Background()
	imgID := seedStorageImage(t, ctx, h.store,
		sc.templateID, sc.poolID, imageSHA256E2E(0xb2), 2048, "qcow2")

	tok := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/storage-pools/"+sc.poolID.String()+"/images/"+imgID.String(), tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := deleter.calls.Load(); got != 1 {
		t.Errorf("agent calls = %d, want 1 (count == 0 path must invoke agent)", got)
	}
	if _, err := h.store.Queries().GetStorageImageByID(ctx, imgID); err == nil {
		t.Errorf("image still present after delete")
	}
}

func TestE2E_StorageImages_Delete_CountZeroAgent5xxRollsBack(t *testing.T) {
	deleter := &recordingImageDeleter{}
	deleter.setError(&agentclient.AgentError{Status: http.StatusServiceUnavailable, Code: "internal", Message: "fs busy"})
	h := newE2EWithImageDeleter(t, deleter)
	defer h.close()

	sc := setupImageScenario(t, h)
	ctx := context.Background()
	imgID := seedStorageImage(t, ctx, h.store,
		sc.templateID, sc.poolID, imageSHA256E2E(0xc3), 4096, "qcow2")

	tok := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/storage-pools/"+sc.poolID.String()+"/images/"+imgID.String(), tok)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeAgentUnreachable {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeAgentUnreachable)
	}
	// Tx rolled back — the row must still exist.
	if _, err := h.store.Queries().GetStorageImageByID(ctx, imgID); err != nil {
		t.Errorf("row missing after agent_unreachable; want preserved by rollback: %v", err)
	}
}

func TestE2E_StorageImages_Delete_AgentDeleterNilEmits502(t *testing.T) {
	// nil ImageDeleter → handler emits 502 on the count == 0 path.
	h := newE2EWithImageDeleter(t, nil)
	defer h.close()

	sc := setupImageScenario(t, h)
	ctx := context.Background()
	imgID := seedStorageImage(t, ctx, h.store,
		sc.templateID, sc.poolID, imageSHA256E2E(0xd4), 1, "qcow2")

	tok := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/storage-pools/"+sc.poolID.String()+"/images/"+imgID.String(), tok)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeAgentUnreachable {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeAgentUnreachable)
	}
	if _, err := h.store.Queries().GetStorageImageByID(ctx, imgID); err != nil {
		t.Errorf("row missing after agent_unreachable; want preserved: %v", err)
	}
}

func TestE2E_StorageImages_Delete_MissingID404(t *testing.T) {
	deleter := &recordingImageDeleter{}
	h := newE2EWithImageDeleter(t, deleter)
	defer h.close()

	sc := setupImageScenario(t, h)
	tok := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/storage-pools/"+sc.poolID.String()+"/images/"+uuid.NewString(), tok)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if got := deleter.calls.Load(); got != 0 {
		t.Errorf("agent calls = %d, want 0 (404 must not invoke agent)", got)
	}
}

func TestE2E_StorageImages_Delete_CrossPool404(t *testing.T) {
	deleter := &recordingImageDeleter{}
	h := newE2EWithImageDeleter(t, deleter)
	defer h.close()

	sc := setupImageScenario(t, h)
	ctx := context.Background()
	imgID := seedStorageImage(t, ctx, h.store,
		sc.templateID, sc.poolID, imageSHA256E2E(0xe5), 1, "qcow2")
	otherPoolStr := createPoolAsAdmin(t, h, sc.nodeID, uniquePoolNameE2E("cross-del"))

	tok := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/storage-pools/"+otherPoolStr+"/images/"+imgID.String(), tok)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_StorageImages_Delete_RBACViewerDenied403(t *testing.T) {
	deleter := &recordingImageDeleter{}
	h := newE2EWithImageDeleter(t, deleter)
	defer h.close()

	sc := setupImageScenario(t, h)
	ctx := context.Background()
	imgID := seedStorageImage(t, ctx, h.store,
		sc.templateID, sc.poolID, imageSHA256E2E(0xf6), 1, "qcow2")

	viewerTok := loginAs(t, h, auth.RoleViewer)
	resp := h.delete(t, "/v1/storage-pools/"+sc.poolID.String()+"/images/"+imgID.String(), viewerTok)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (viewer lacks storage_image:manage)", resp.StatusCode)
	}
}

func TestE2E_StorageImages_Delete_DeveloperOwnSucceeds(t *testing.T) {
	deleter := &recordingImageDeleter{}
	h := newE2EWithImageDeleter(t, deleter)
	defer h.close()

	sc := setupImageScenario(t, h)
	ctx := context.Background()
	// sc.templateID is owned by sc.ownerID who is also sc.ownerToken's caller.
	imgID := seedStorageImage(t, ctx, h.store,
		sc.templateID, sc.poolID, imageSHA256E2E(0x07), 1, "qcow2")

	resp := h.delete(t, "/v1/storage-pools/"+sc.poolID.String()+"/images/"+imgID.String(), sc.ownerToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (developer owns template)", resp.StatusCode)
	}
}

func TestE2E_StorageImages_Delete_DeveloperPublicBypassSucceeds(t *testing.T) {
	deleter := &recordingImageDeleter{}
	h := newE2EWithImageDeleter(t, deleter)
	defer h.close()

	ctx := context.Background()

	// Owner is admin (so they can publish). Caller is a different developer.
	adminTok := loginAs(t, h, auth.RoleAdmin)
	tplID := createTemplate(t, h, templateBody(uniqueTemplateName("pubdel")), adminTok)
	publishTemplate(t, h, tplID, adminTok)
	tplUUID := uuid.MustParse(lookupTemplateID(t, h, tplID))

	nodeID := seedNodeForE2E(t, ctx, h.store)
	poolStr := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("pubpool"))
	poolID := uuid.MustParse(poolStr)
	imgID := seedStorageImage(t, ctx, h.store, tplUUID, poolID, imageSHA256E2E(0x18), 1, "qcow2")

	_, devTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)
	resp := h.delete(t, "/v1/storage-pools/"+poolID.String()+"/images/"+imgID.String(), devTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (developer covered by public-bypass)", resp.StatusCode)
	}
}

func TestE2E_StorageImages_Delete_DeveloperPrivateOther403(t *testing.T) {
	deleter := &recordingImageDeleter{}
	h := newE2EWithImageDeleter(t, deleter)
	defer h.close()

	ctx := context.Background()

	// Template owned by user A (admin); private (default visibility).
	adminTok := loginAs(t, h, auth.RoleAdmin)
	tplID := createTemplate(t, h, templateBody(uniqueTemplateName("privdel")), adminTok)
	tplUUID := uuid.MustParse(lookupTemplateID(t, h, tplID))

	nodeID := seedNodeForE2E(t, ctx, h.store)
	poolStr := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("privpool"))
	poolID := uuid.MustParse(poolStr)
	imgID := seedStorageImage(t, ctx, h.store, tplUUID, poolID, imageSHA256E2E(0x29), 1, "qcow2")

	_, devTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)
	resp := h.delete(t, "/v1/storage-pools/"+poolID.String()+"/images/"+imgID.String(), devTok)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (composite check fails)", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodePermissionDenied {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodePermissionDenied)
	}
}

func TestE2E_StorageImages_Delete_AnonRejected401(t *testing.T) {
	deleter := &recordingImageDeleter{}
	h := newE2EWithImageDeleter(t, deleter)
	defer h.close()

	sc := setupImageScenario(t, h)
	ctx := context.Background()
	imgID := seedStorageImage(t, ctx, h.store,
		sc.templateID, sc.poolID, imageSHA256E2E(0x3a), 1, "qcow2")

	resp := h.delete(t, "/v1/storage-pools/"+sc.poolID.String()+"/images/"+imgID.String(), "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestE2E_StorageImages_Delete_IdempotencyReplay(t *testing.T) {
	deleter := &recordingImageDeleter{}
	h := newE2EWithImageDeleter(t, deleter)
	defer h.close()

	sc := setupImageScenario(t, h)
	ctx := context.Background()
	imgID := seedStorageImage(t, ctx, h.store,
		sc.templateID, sc.poolID, imageSHA256E2E(0x4b), 1, "qcow2")

	tok := loginAs(t, h, auth.RoleAdmin)
	idemKey := "idem-" + uuid.NewString()

	resp1 := h.deleteWithIdempotency(t, "/v1/storage-pools/"+sc.poolID.String()+"/images/"+imgID.String(), tok, idemKey)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first delete status = %d, want 200", resp1.StatusCode)
	}
	if got := deleter.calls.Load(); got != 1 {
		t.Errorf("agent calls after first = %d, want 1", got)
	}

	resp2 := h.deleteWithIdempotency(t, "/v1/storage-pools/"+sc.poolID.String()+"/images/"+imgID.String(), tok, idemKey)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d, want 200 (idempotency middleware replays cached 200)", resp2.StatusCode)
	}
	// Replay must NOT re-invoke the agent — handler did not re-run.
	if got := deleter.calls.Load(); got != 1 {
		t.Errorf("agent calls after replay = %d, want 1 (handler replayed from cache)", got)
	}
}

// ----- storage_image.import handler e2e tests ------

// importBody returns a minimal valid request body. Tests vary it to
// cover validation branches.
func importBody(poolID string) map[string]any {
	return map[string]any{"pool": poolID}
}

func TestE2E_StorageImages_Import_HappyPath(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	resp := h.post(t, "/v1/templates/"+sc.templateName+"/images",
		importBody(sc.poolID.String()), sc.ownerToken)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var accepted struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
		Links  struct {
			Self string `json:"self"`
		} `json:"links"`
	}
	decodeJSON(t, resp, &accepted)
	if accepted.TaskID == "" {
		t.Fatal("task_id empty")
	}
	if accepted.Status != "pending" {
		t.Errorf("status = %q, want pending", accepted.Status)
	}
	if accepted.Links.Self != "/v1/tasks/"+accepted.TaskID {
		t.Errorf("links.self = %q, want /v1/tasks/%s", accepted.Links.Self, accepted.TaskID)
	}

	// Task row materialised with the correct type / resource.
	taskUUID, err := uuid.Parse(accepted.TaskID)
	if err != nil {
		t.Fatalf("parse task uuid: %v", err)
	}
	task, err := h.store.Queries().GetTask(context.Background(), taskUUID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Type != "storage_image.import" {
		t.Errorf("Task.Type = %q, want storage_image.import", task.Type)
	}
	if task.Status != store.TaskStatusPending {
		t.Errorf("Task.Status = %q, want pending", task.Status)
	}
	if task.ResourceType != "template" {
		t.Errorf("Task.ResourceType = %q, want template", task.ResourceType)
	}
	if task.ResourceID == nil || *task.ResourceID != sc.templateID {
		t.Errorf("Task.ResourceID = %v, want %s", task.ResourceID, sc.templateID)
	}
	if task.CreatedBy == nil || *task.CreatedBy != sc.ownerID {
		t.Errorf("Task.CreatedBy = %v, want %s", task.CreatedBy, sc.ownerID)
	}
	if task.RiverJobID == nil || *task.RiverJobID == 0 {
		t.Errorf("Task.RiverJobID = %v, want non-zero (atomic enqueue)", task.RiverJobID)
	}
}

func TestE2E_StorageImages_Import_BadBody(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	cases := []struct {
		name       string
		body       map[string]any
		wantStatus int
		wantCode   response.ErrorCode
	}{
		{
			name:       "missing pool",
			body:       map[string]any{},
			wantStatus: http.StatusBadRequest,
			wantCode:   response.CodeValidationFailed,
		},
		// `pool` is a polymorphic identifier: a non-UUID value is
		// treated as a name lookup and surfaces as 404 (no leak)
		// rather than a 400 validation envelope.
		{
			name:       "unknown pool name",
			body:       map[string]any{"pool": "not-a-pool"},
			wantStatus: http.StatusNotFound,
			wantCode:   response.CodeNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.post(t, "/v1/templates/"+sc.templateName+"/images", tc.body, sc.ownerToken)
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			var b response.ErrorBody
			decodeJSON(t, resp, &b)
			if b.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", b.Error.Code, tc.wantCode)
			}
		})
	}
}

// TestE2E_StorageImages_Import_UnknownTemplateName404 — the template
// path identifier is polymorphic: a non-UUID value resolves through a
// name lookup and surfaces as 404, not 400.
func TestE2E_StorageImages_Import_UnknownTemplateName404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	resp := h.post(t, "/v1/templates/not-a-template/images",
		importBody(sc.poolID.String()), sc.ownerToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_StorageImages_Import_TemplateNotFound(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	resp := h.post(t, "/v1/templates/no-such-template-name"+"/images",
		importBody(sc.poolID.String()), sc.ownerToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_StorageImages_Import_PoolNotFound(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	resp := h.post(t, "/v1/templates/"+sc.templateName+"/images",
		importBody(uuid.NewString()), sc.ownerToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_StorageImages_Import_NodeNotScannable409(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()
	sc := setupImageScenario(t, h)

	// Force the owning node into `unreachable` — not in the scannable set.
	// sc.nodeID carries the node *name* (seedNodeForE2E returns the
	// name); flip status by name through the store.
	if _, err := h.store.Pool().Exec(ctx,
		`update nodes set status = 'unreachable' where lower(name) = lower($1)`, sc.nodeID); err != nil {
		t.Fatalf("flip node status: %v", err)
	}

	resp := h.post(t, "/v1/templates/"+sc.templateName+"/images",
		importBody(sc.poolID.String()), sc.ownerToken)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeConflict {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodeConflict)
	}
	if got, _ := b.Error.Details["current_status"].(string); got != "unreachable" {
		t.Errorf("current_status = %v, want unreachable", got)
	}
}

func TestE2E_StorageImages_Import_DeveloperOwnSucceeds(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	// sc.ownerToken is a developer; sc.templateID is owned by sc.ownerID.
	resp := h.post(t, "/v1/templates/"+sc.templateName+"/images",
		importBody(sc.poolID.String()), sc.ownerToken)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (developer owns template)", resp.StatusCode)
	}
}

func TestE2E_StorageImages_Import_DeveloperPublicBypassSucceeds(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()

	adminTok := loginAs(t, h, auth.RoleAdmin)
	tplID := createTemplate(t, h, templateBody(uniqueTemplateName("pubimp")), adminTok)
	publishTemplate(t, h, tplID, adminTok)

	nodeID := seedNodeForE2E(t, ctx, h.store)
	poolStr := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("pubpool"))

	_, devTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)
	resp := h.post(t, "/v1/templates/"+tplID+"/images",
		importBody(poolStr), devTok)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (public-bypass)", resp.StatusCode)
	}
}

func TestE2E_StorageImages_Import_DeveloperPrivateOther403(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	ctx := context.Background()

	adminTok := loginAs(t, h, auth.RoleAdmin)
	tplID := createTemplate(t, h, templateBody(uniqueTemplateName("privimp")), adminTok)

	nodeID := seedNodeForE2E(t, ctx, h.store)
	poolStr := createPoolAsAdmin(t, h, nodeID, uniquePoolNameE2E("privpool"))

	_, devTok := loginAsReturningUserID(t, h, auth.RoleDeveloper)
	resp := h.post(t, "/v1/templates/"+tplID+"/images",
		importBody(poolStr), devTok)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (composite check fails)", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodePermissionDenied {
		t.Errorf("code = %q, want %q", b.Error.Code, response.CodePermissionDenied)
	}
}

func TestE2E_StorageImages_Import_ViewerDenied403(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	viewerTok := loginAs(t, h, auth.RoleViewer)
	resp := h.post(t, "/v1/templates/"+sc.templateName+"/images",
		importBody(sc.poolID.String()), viewerTok)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (viewer lacks storage_image:import)", resp.StatusCode)
	}
}

func TestE2E_StorageImages_Import_AnonRejected401(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	resp := h.post(t, "/v1/templates/"+sc.templateName+"/images",
		importBody(sc.poolID.String()), "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestE2E_StorageImages_Import_IdempotencyReplay(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	sc := setupImageScenario(t, h)
	idemKey := "idem-" + uuid.NewString()

	resp1 := h.postWithHeaders(t, "/v1/templates/"+sc.templateName+"/images",
		importBody(sc.poolID.String()), sc.ownerToken,
		map[string]string{"Idempotency-Key": idemKey})
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", resp1.StatusCode)
	}
	var first map[string]any
	decodeJSON(t, resp1, &first)

	resp2 := h.postWithHeaders(t, "/v1/templates/"+sc.templateName+"/images",
		importBody(sc.poolID.String()), sc.ownerToken,
		map[string]string{"Idempotency-Key": idemKey})
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("replay status = %d, want 202 (idempotency middleware replays)", resp2.StatusCode)
	}
	var second map[string]any
	decodeJSON(t, resp2, &second)
	if first["task_id"] != second["task_id"] {
		t.Errorf("replay task_id mismatch: first=%v second=%v", first["task_id"], second["task_id"])
	}
}
