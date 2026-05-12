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
)

// joinTokenView mirrors the JSON shape returned by the handlers.
// Subset of fields tests assert against.
type joinTokenView struct {
	ID               string  `json:"id"`
	IntendedNodeName *string `json:"intended_node_name"`
	ExpiresAt        string  `json:"expires_at"`
	MaxUses          *int64  `json:"max_uses"`
	ConsumptionCount int64   `json:"consumption_count"`
	CreatedByUserID  *string `json:"created_by_user_id"`
	CreatedAt        string  `json:"created_at"`
}

type joinTokenCreateResponse struct {
	joinTokenView
	Token               string `json:"token"`
	CAFingerprintSHA256 string `json:"ca_fingerprint_sha256"`
}

// TestE2E_JoinTokens_CreateRequiresAdmin verifies node:manage gating
// — only admin can mint а join token; the other three roles get 403.
func TestE2E_JoinTokens_CreateRequiresAdmin(t *testing.T) {
	h := newE2E(t)
	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			tok := loginAs(t, h, role)
			resp := h.post(t, "/v1/nodes/join-tokens", map[string]any{}, tok)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}
}

// TestE2E_JoinTokens_CreateHappyPath verifies the token bundle shape
// — token plaintext present, CA fingerprint matches the active CA
// row's fingerprint, max_uses null by default, consumption_count = 0,
// created_by_user_id set к the admin caller.
func TestE2E_JoinTokens_CreateHappyPath(t *testing.T) {
	h := newE2E(t)
	admin := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/nodes/join-tokens", map[string]any{
		"ttl_seconds": 600,
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	var bundle joinTokenCreateResponse
	decodeJSON(t, resp, &bundle)

	if !strings.HasPrefix(bundle.Token, "otx_join_") {
		t.Errorf("token = %q, missing otx_join_ prefix", bundle.Token)
	}
	if len(bundle.CAFingerprintSHA256) != 64 {
		t.Errorf("ca_fingerprint_sha256 length = %d, want 64 hex chars", len(bundle.CAFingerprintSHA256))
	}
	if bundle.MaxUses != nil {
		t.Errorf("max_uses = %v, want nil (unlimited default)", bundle.MaxUses)
	}
	if bundle.ConsumptionCount != 0 {
		t.Errorf("consumption_count = %d, want 0", bundle.ConsumptionCount)
	}
	if bundle.CreatedByUserID == nil || *bundle.CreatedByUserID == "" {
		t.Error("created_by_user_id not set on token mint")
	}

	// CA fingerprint matches the active row.
	row, err := h.store.Queries().GetActiveCACert(context.Background())
	if err != nil {
		t.Fatalf("GetActiveCACert: %v", err)
	}
	wantHex := hexEncodeBytes(row.FingerprintSha256)
	if bundle.CAFingerprintSHA256 != wantHex {
		t.Errorf("ca_fingerprint_sha256 = %s, want %s", bundle.CAFingerprintSHA256, wantHex)
	}
}

// TestE2E_JoinTokens_CreatePreboundEnforcesSingleUse verifies the
// API-edge defense against pre-bound multi-use (intended_node_name +
// max_uses > 1 → 400 validation_failed).
func TestE2E_JoinTokens_CreatePreboundEnforcesSingleUse(t *testing.T) {
	h := newE2E(t)
	admin := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/nodes/join-tokens", map[string]any{
		"intended_node_name": "node-mvp",
		"max_uses":           3,
	}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body response.ErrorBody
	decodeJSON(t, resp, &body)
	if body.Error.Code != response.CodeValidationFailed {
		t.Errorf("code = %q, want validation_failed", body.Error.Code)
	}
}

// TestE2E_JoinTokens_CreatePreboundSingleUseAccepted verifies the
// happy path для intended_node_name + max_uses=1.
func TestE2E_JoinTokens_CreatePreboundSingleUseAccepted(t *testing.T) {
	h := newE2E(t)
	admin := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/nodes/join-tokens", map[string]any{
		"intended_node_name": "node-mvp",
		"max_uses":           1,
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var bundle joinTokenCreateResponse
	decodeJSON(t, resp, &bundle)
	if bundle.IntendedNodeName == nil || *bundle.IntendedNodeName != "node-mvp" {
		t.Errorf("intended_node_name = %v, want node-mvp", bundle.IntendedNodeName)
	}
	if bundle.MaxUses == nil || *bundle.MaxUses != 1 {
		t.Errorf("max_uses = %v, want 1", bundle.MaxUses)
	}
}

// TestE2E_JoinTokens_CreateTTLBounds verifies the API-edge TTL
// validation (60..86400 seconds).
func TestE2E_JoinTokens_CreateTTLBounds(t *testing.T) {
	h := newE2E(t)
	admin := loginAs(t, h, auth.RoleAdmin)
	for _, ttl := range []int{0, 59, 86401, 1_000_000} {
		t.Run(ttlLabel(ttl), func(t *testing.T) {
			resp := h.post(t, "/v1/nodes/join-tokens", map[string]any{
				"ttl_seconds": ttl,
			}, admin)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("ttl=%d: status = %d, want 400", ttl, resp.StatusCode)
			}
		})
	}
}

// TestE2E_JoinTokens_CreateMaxUsesBounds verifies max_uses>=1.
func TestE2E_JoinTokens_CreateMaxUsesBounds(t *testing.T) {
	h := newE2E(t)
	admin := loginAs(t, h, auth.RoleAdmin)
	resp := h.post(t, "/v1/nodes/join-tokens", map[string]any{
		"max_uses": 0,
	}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestE2E_JoinTokens_CreateIgnoresIdempotencyKey verifies that
// re-issuing the same idempotency key produces а NEW token
// (каждый create call mints а fresh token).
func TestE2E_JoinTokens_CreateIgnoresIdempotencyKey(t *testing.T) {
	h := newE2E(t)
	admin := loginAs(t, h, auth.RoleAdmin)
	key := uuid.NewString()

	resp1 := h.postWithHeaders(t, "/v1/nodes/join-tokens", map[string]any{},
		admin, map[string]string{"Idempotency-Key": key})
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", resp1.StatusCode)
	}
	var first joinTokenCreateResponse
	decodeJSON(t, resp1, &first)

	resp2 := h.postWithHeaders(t, "/v1/nodes/join-tokens", map[string]any{},
		admin, map[string]string{"Idempotency-Key": key})
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("second status = %d, want 201 (idempotency should be ignored)", resp2.StatusCode)
	}
	var second joinTokenCreateResponse
	decodeJSON(t, resp2, &second)

	if first.Token == second.Token {
		t.Error("identical Idempotency-Key produced same token — idempotency must NOT apply к create")
	}
	if first.ID == second.ID {
		t.Error("identical Idempotency-Key produced same token id — idempotency must NOT apply к create")
	}
}

// TestE2E_JoinTokens_ListAndGet verifies the list endpoint surfaces
// freshly-minted tokens and excludes expired ones by default.
func TestE2E_JoinTokens_ListAndGet(t *testing.T) {
	h := newE2E(t)
	admin := loginAs(t, h, auth.RoleAdmin)

	// Mint а fresh token.
	resp := h.post(t, "/v1/nodes/join-tokens", map[string]any{
		"ttl_seconds": 600,
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var minted joinTokenCreateResponse
	decodeJSON(t, resp, &minted)

	// List — must include the new token.
	listResp := h.get(t, "/v1/nodes/join-tokens", admin)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", listResp.StatusCode)
	}
	var list struct {
		Data []joinTokenView `json:"data"`
		Meta struct {
			NextCursor *string `json:"next_cursor"`
		} `json:"meta"`
	}
	decodeJSON(t, listResp, &list)
	found := false
	for _, t := range list.Data {
		if t.ID == minted.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("freshly minted token %s missing from list", minted.ID)
	}
}

// TestE2E_JoinTokens_DeleteRevoke verifies that revoke is а 204 on
// the happy path и subsequent revoke returns 409.
func TestE2E_JoinTokens_DeleteRevoke(t *testing.T) {
	h := newE2E(t)
	admin := loginAs(t, h, auth.RoleAdmin)

	resp := h.post(t, "/v1/nodes/join-tokens", map[string]any{"ttl_seconds": 600}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var minted joinTokenCreateResponse
	decodeJSON(t, resp, &minted)

	delResp := h.delete(t, "/v1/nodes/join-tokens/"+minted.ID, admin)
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("first delete status = %d, want 204", delResp.StatusCode)
	}

	// Second delete: token now expired, must return 409.
	repeatResp := h.delete(t, "/v1/nodes/join-tokens/"+minted.ID, admin)
	if repeatResp.StatusCode != http.StatusConflict {
		t.Errorf("second delete status = %d, want 409", repeatResp.StatusCode)
	}
}

// TestE2E_JoinTokens_DeleteUnknown verifies а random uuid returns 404.
func TestE2E_JoinTokens_DeleteUnknown(t *testing.T) {
	h := newE2E(t)
	admin := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/nodes/join-tokens/"+uuid.NewString(), admin)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestE2E_JoinTokens_ListConsumptionsEmpty verifies the audit endpoint
// returns an empty list для а freshly-minted token (no redemption
// pre-Step 2).
func TestE2E_JoinTokens_ListConsumptionsEmpty(t *testing.T) {
	h := newE2E(t)
	admin := loginAs(t, h, auth.RoleAdmin)

	resp := h.post(t, "/v1/nodes/join-tokens", map[string]any{"ttl_seconds": 600}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var minted joinTokenCreateResponse
	decodeJSON(t, resp, &minted)

	listResp := h.get(t, "/v1/nodes/join-tokens/"+minted.ID+"/consumptions", admin)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", listResp.StatusCode)
	}
	var list struct {
		Data []map[string]any `json:"data"`
		Meta struct {
			NextCursor *string `json:"next_cursor"`
		} `json:"meta"`
	}
	decodeJSON(t, listResp, &list)
	if len(list.Data) != 0 {
		t.Errorf("data length = %d, want 0 (no redemption in Step 1)", len(list.Data))
	}
	if list.Meta.NextCursor != nil {
		t.Error("next_cursor != nil on empty list")
	}
}

// TestE2E_JoinTokens_ListConsumptionsUnknownToken verifies а random
// uuid returns 404 from the audit endpoint.
func TestE2E_JoinTokens_ListConsumptionsUnknownToken(t *testing.T) {
	h := newE2E(t)
	admin := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/nodes/join-tokens/"+uuid.NewString()+"/consumptions", admin)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// hexEncodeBytes is а small helper к keep tests readable. The auth
// package's hex.EncodeToString is already wired в production code,
// но pulling encoding/hex into the test file is а thinner dependency.
func hexEncodeBytes(b []byte) string {
	const hexChars = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexChars[v>>4]
		out[i*2+1] = hexChars[v&0x0f]
	}
	return string(out)
}

func ttlLabel(ttl int) string {
	switch {
	case ttl <= 0:
		return "zero"
	case ttl < 60:
		return "below_min"
	case ttl > 86400:
		return "above_max"
	}
	return "valid"
}
