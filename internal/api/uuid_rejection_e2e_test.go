// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// TestE2E_UUIDRejection_PathParams locks the path narrowing
// behaviour: every operator-facing name-only path returns 400
// validation_failed when а UUID literal is supplied. The structured
// `details` payload must carry (field, reason, hint) so CLI scripts
// can branch programmatically without parsing the human-readable
// message.
//
// Storage pool paths are intentionally NOT covered here — they
// retain UUID acceptance as а multi-instance carve-out (`pool get
// <uuid>` returns the flat per-node instance; addressing via UUID is
// essential когда the pool has more than one row).
func TestE2E_UUIDRejection_PathParams(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminTok := loginAs(t, h, auth.RoleAdmin)
	bogusID := uuid.NewString()

	cases := []struct {
		name string
		path string
	}{
		{"vm get by uuid", "/v1/vms/" + bogusID},
		{"template get by uuid", "/v1/templates/" + bogusID},
		{"node get by uuid", "/v1/nodes/" + bogusID},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.get(t, tc.path, adminTok)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			var b response.ErrorBody
			decodeJSON(t, resp, &b)
			if b.Error.Code != response.CodeValidationFailed {
				t.Errorf("code = %s, want validation_failed", b.Error.Code)
			}
			reason, _ := b.Error.Details["reason"].(string)
			if reason != "uuid_not_accepted_in_name_path" {
				t.Errorf("details.reason = %q, want uuid_not_accepted_in_name_path", reason)
			}
			hint, _ := b.Error.Details["hint"].(string)
			if !strings.Contains(hint, "list") {
				t.Errorf("details.hint = %q, want substring 'list'", hint)
			}
		})
	}
}

// TestE2E_UUIDRejection_VMCreateBody covers the body field narrowing
// for vm create: `template` rejects а UUID literal с the canonical
// 400 envelope. `pool` retains а carve-out — passing а pool UUID
// still works (the handler accepts both forms по the scheduler-input
// pre-translation step in vms/create.go) so it is NOT exercised
// here.
func TestE2E_UUIDRejection_VMCreateBody(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	devTok := loginAs(t, h, auth.RoleDeveloper)

	body := map[string]any{
		"name":      "uuid-reject-vm-" + uuid.NewString()[:8],
		"template":  uuid.NewString(),
		"pool":      "pool-mvp",
		"vcpus":     2,
		"memory_mb": 2048,
	}
	resp := h.post(t, "/v1/vms", body, devTok)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeValidationFailed {
		t.Errorf("code = %s, want validation_failed", b.Error.Code)
	}
	if field, _ := b.Error.Details["field"].(string); field != "template" {
		t.Errorf("details.field = %q, want template", field)
	}
}

// TestE2E_UUIDRejection_StoragePoolCreateBody covers the storage_pools
// create body's `node` field — name-only.
func TestE2E_UUIDRejection_StoragePoolCreateBody(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminTok := loginAs(t, h, auth.RoleAdmin)

	body := map[string]any{
		"node": uuid.NewString(),
		"name": uniquePoolNameE2E("uuid-reject"),
		"type": "local_dir",
		"path": "/opt/otherix/pools/uuid-reject",
	}
	resp := h.post(t, "/v1/storage-pools", body, adminTok)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeValidationFailed {
		t.Errorf("code = %s, want validation_failed", b.Error.Code)
	}
	if field, _ := b.Error.Details["field"].(string); field != "node" {
		t.Errorf("details.field = %q, want node", field)
	}
}

// TestE2E_UUIDRejection_VMListNodeFilter covers the vms.list ?node=
// query filter. Pool filter is NOT covered (carve-out — accepts
// UUIDs via resolver.Pool's polymorphic branch).
func TestE2E_UUIDRejection_VMListNodeFilter(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminTok := loginAs(t, h, auth.RoleAdmin)

	resp := h.get(t, "/v1/vms?node="+uuid.NewString(), adminTok)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var b response.ErrorBody
	decodeJSON(t, resp, &b)
	if b.Error.Code != response.CodeValidationFailed {
		t.Errorf("code = %s, want validation_failed", b.Error.Code)
	}
}
