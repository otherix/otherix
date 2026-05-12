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
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// adminApiTokenCreated is the shape returned by POST .../api-tokens.
// Embeds the read-only ApiToken view plus the plaintext token.
type adminApiTokenCreated struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	Token     string `json:"token"`
	RevokedAt any    `json:"revoked_at"`
	ExpiresAt any    `json:"expires_at"`
}

type adminApiTokenView struct {
	ID        string  `json:"id"`
	UserID    string  `json:"user_id"`
	Name      string  `json:"name"`
	Prefix    string  `json:"prefix"`
	Token     string  `json:"token"` // expected absent → empty
	RevokedAt *string `json:"revoked_at"`
}

type adminApiTokenList struct {
	Data []adminApiTokenView `json:"data"`
	Meta struct {
		NextCursor *string `json:"next_cursor"`
	} `json:"meta"`
}

// loginAsReturningID returns the seeded user's id alongside the access
// token. Tests need both because admin-on-behalf endpoints take a
// {user_id} path parameter.
func loginAsReturningID(t *testing.T, h *e2eHarness, role auth.Role) (uuid.UUID, string) {
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

// adminCreateTokenFor seeds an api-token for targetID via the admin
// endpoint and returns the plaintext + id.
func adminCreateTokenFor(t *testing.T, h *e2eHarness, adminToken string, targetID uuid.UUID, name string) adminApiTokenCreated {
	t.Helper()
	resp := h.post(t, "/v1/users/"+targetID.String()+"/api-tokens",
		map[string]string{"name": name}, adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin create token: status = %d, want 201", resp.StatusCode)
	}
	var created adminApiTokenCreated
	decodeJSON(t, resp, &created)
	return created
}

// TestE2E_AdminApiTokens_CreateRBAC asserts that admin may create a
// token for any user, and a non-admin caller targeting another user's
// id receives 404 (no existence leak). Self-targeting works for every
// role under scope=own.
func TestE2E_AdminApiTokens_CreateRBAC(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminID, adminToken := loginAsReturningID(t, h, auth.RoleAdmin)
	otherID, _ := loginAsReturningID(t, h, auth.RoleDeveloper)

	// Admin → other user: 201, returned user_id is target's.
	resp := h.post(t, "/v1/users/"+otherID.String()+"/api-tokens",
		map[string]string{"name": "admin-on-behalf"}, adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin cross-user create: status = %d, want 201", resp.StatusCode)
	}
	var created adminApiTokenCreated
	decodeJSON(t, resp, &created)
	if created.UserID != otherID.String() {
		t.Errorf("created.user_id = %q, want %q (target id)", created.UserID, otherID.String())
	}
	if !strings.HasPrefix(created.Token, "otx_") {
		t.Errorf("token = %q, want otx_ prefix", created.Token)
	}

	// Admin → self: 201.
	resp = h.post(t, "/v1/users/"+adminID.String()+"/api-tokens",
		map[string]string{"name": "admin-self"}, adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("admin self-target create: status = %d, want 201", resp.StatusCode)
	}

	// Non-admin → other user: 404.
	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run("cross-user-"+string(role), func(t *testing.T) {
			_, callerToken := loginAsReturningID(t, h, role)
			resp := h.post(t, "/v1/users/"+otherID.String()+"/api-tokens",
				map[string]string{"name": "should-fail"}, callerToken)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404 (no existence leak)", resp.StatusCode)
			}
			var body response.ErrorBody
			decodeJSON(t, resp, &body)
			if body.Error.Code != response.CodeNotFound {
				t.Errorf("code = %q, want %q", body.Error.Code, response.CodeNotFound)
			}
		})
	}

	// Non-admin → self: 201.
	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run("self-"+string(role), func(t *testing.T) {
			callerID, callerToken := loginAsReturningID(t, h, role)
			resp := h.post(t, "/v1/users/"+callerID.String()+"/api-tokens",
				map[string]string{"name": "self-ok"}, callerToken)
			if resp.StatusCode != http.StatusCreated {
				t.Errorf("status = %d, want 201", resp.StatusCode)
			}
		})
	}
}

// TestE2E_AdminApiTokens_ListRBAC mirrors the create RBAC test for the
// list endpoint and confirms cross-user 404 for non-admin.
func TestE2E_AdminApiTokens_ListRBAC(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	_, adminToken := loginAsReturningID(t, h, auth.RoleAdmin)
	otherID, _ := loginAsReturningID(t, h, auth.RoleDeveloper)
	adminCreateTokenFor(t, h, adminToken, otherID, "seeded")

	// Admin sees other user's tokens.
	resp := h.get(t, "/v1/users/"+otherID.String()+"/api-tokens", adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin cross-user list: status = %d, want 200", resp.StatusCode)
	}
	var list adminApiTokenList
	decodeJSON(t, resp, &list)
	if len(list.Data) == 0 {
		t.Errorf("admin list returned empty data; want >=1")
	}
	for _, v := range list.Data {
		if v.UserID != otherID.String() {
			t.Errorf("list returned token for user_id %q, want %q", v.UserID, otherID.String())
		}
		if v.Token != "" {
			t.Errorf("list leaked plaintext token: %q", v.Token)
		}
	}

	// Non-admin → other user: 404.
	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			_, callerToken := loginAsReturningID(t, h, role)
			resp := h.get(t, "/v1/users/"+otherID.String()+"/api-tokens", callerToken)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", resp.StatusCode)
			}
		})
	}
}

// TestE2E_AdminApiTokens_DeleteRBAC checks that admin may revoke any
// user's token via the admin path; non-admin targeting another user
// gets 404.
func TestE2E_AdminApiTokens_DeleteRBAC(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	_, adminToken := loginAsReturningID(t, h, auth.RoleAdmin)
	otherID, _ := loginAsReturningID(t, h, auth.RoleDeveloper)

	// Seed a token for `other` via admin so we have a known id.
	created := adminCreateTokenFor(t, h, adminToken, otherID, "to-revoke")

	// Non-admin targeting other user's id → 404.
	for _, role := range []auth.Role{auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			_, callerToken := loginAsReturningID(t, h, role)
			resp := h.delete(t, "/v1/users/"+otherID.String()+"/api-tokens/"+created.ID, callerToken)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", resp.StatusCode)
			}
		})
	}

	// Admin → 204.
	resp := h.delete(t, "/v1/users/"+otherID.String()+"/api-tokens/"+created.ID, adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("admin delete status = %d, want 204", resp.StatusCode)
	}

	// Token no longer authenticates.
	resp = h.get(t, "/v1/users/me", created.Token)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked-token getMe status = %d, want 401", resp.StatusCode)
	}
}

// TestE2E_AdminApiTokens_TargetUserNotFound returns 404 even for
// admin when the target id does not exist. Same for soft-deleted users.
func TestE2E_AdminApiTokens_TargetUserNotFound(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	_, adminToken := loginAsReturningID(t, h, auth.RoleAdmin)
	missing := uuid.NewString()

	resp := h.post(t, "/v1/users/"+missing+"/api-tokens",
		map[string]string{"name": "ghost"}, adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("create on missing user: status = %d, want 404", resp.StatusCode)
	}
	resp = h.get(t, "/v1/users/"+missing+"/api-tokens", adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("list on missing user: status = %d, want 404", resp.StatusCode)
	}
	resp = h.delete(t, "/v1/users/"+missing+"/api-tokens/"+uuid.NewString(), adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete on missing user: status = %d, want 404", resp.StatusCode)
	}
}

// TestE2E_AdminApiTokens_URIMismatchDelete confirms that DELETE
// /v1/users/{a}/api-tokens/{token_belonging_to_b} returns 404 even
// when the caller is an admin — the URI describes a token that does
// not exist at that location.
func TestE2E_AdminApiTokens_URIMismatchDelete(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	_, adminToken := loginAsReturningID(t, h, auth.RoleAdmin)
	userA, _ := loginAsReturningID(t, h, auth.RoleDeveloper)
	userB, _ := loginAsReturningID(t, h, auth.RoleDeveloper)

	tokenForB := adminCreateTokenFor(t, h, adminToken, userB, "for-b")

	// Admin asks to DELETE B's token through A's URI.
	resp := h.delete(t, "/v1/users/"+userA.String()+"/api-tokens/"+tokenForB.ID, adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("URI-mismatch delete: status = %d, want 404", resp.StatusCode)
	}

	// The token is still alive — DELETE under the right URI works.
	resp = h.delete(t, "/v1/users/"+userB.String()+"/api-tokens/"+tokenForB.ID, adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("correct-URI delete: status = %d, want 204", resp.StatusCode)
	}
}

// TestE2E_AdminApiTokens_AlreadyRevokedDelete204 checks that a second
// DELETE on an already-revoked token still emits 204. RevokeApiToken
// is idempotent at the SQL layer.
func TestE2E_AdminApiTokens_AlreadyRevokedDelete204(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	_, adminToken := loginAsReturningID(t, h, auth.RoleAdmin)
	target, _ := loginAsReturningID(t, h, auth.RoleDeveloper)
	created := adminCreateTokenFor(t, h, adminToken, target, "twice")

	resp := h.delete(t, "/v1/users/"+target.String()+"/api-tokens/"+created.ID, adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first delete status = %d, want 204", resp.StatusCode)
	}

	// Without idempotency-key — second DELETE goes through the same
	// handler and should also emit 204.
	resp = h.delete(t, "/v1/users/"+target.String()+"/api-tokens/"+created.ID, adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("second delete status = %d, want 204 (idempotent revoke)", resp.StatusCode)
	}
}

// TestE2E_AdminApiTokens_IncludeRevoked verifies that the
// `?include_revoked` query parameter filters revoked tokens out by
// default and surfaces them when set to true.
func TestE2E_AdminApiTokens_IncludeRevoked(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	_, adminToken := loginAsReturningID(t, h, auth.RoleAdmin)
	target, _ := loginAsReturningID(t, h, auth.RoleDeveloper)

	live := adminCreateTokenFor(t, h, adminToken, target, "live")
	dead := adminCreateTokenFor(t, h, adminToken, target, "dead")

	resp := h.delete(t, "/v1/users/"+target.String()+"/api-tokens/"+dead.ID, adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("seed revoke: status = %d, want 204", resp.StatusCode)
	}

	// Default (include_revoked omitted) — only `live`.
	resp = h.get(t, "/v1/users/"+target.String()+"/api-tokens", adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("default list status = %d, want 200", resp.StatusCode)
	}
	var def adminApiTokenList
	decodeJSON(t, resp, &def)
	if !containsTokenID(def.Data, live.ID) {
		t.Errorf("default list missing live token %q", live.ID)
	}
	if containsTokenID(def.Data, dead.ID) {
		t.Errorf("default list surfaced revoked token %q (want hidden)", dead.ID)
	}

	// include_revoked=true — both.
	resp = h.get(t, "/v1/users/"+target.String()+"/api-tokens?include_revoked=true", adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("include_revoked=true list status = %d, want 200", resp.StatusCode)
	}
	var all adminApiTokenList
	decodeJSON(t, resp, &all)
	if !containsTokenID(all.Data, live.ID) || !containsTokenID(all.Data, dead.ID) {
		t.Errorf("include_revoked=true list missing tokens — live=%v dead=%v",
			containsTokenID(all.Data, live.ID), containsTokenID(all.Data, dead.ID))
	}
}

// TestE2E_MeApiTokens_IncludeRevokedDefault confirms variant B is
// active on the /me list endpoint too: revoked tokens are hidden by
// default, surfaced under ?include_revoked=true.
func TestE2E_MeApiTokens_IncludeRevokedDefault(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	_, callerToken := loginAsReturningID(t, h, auth.RoleDeveloper)

	// Create two tokens, revoke one, then list as the user themselves.
	resp := h.post(t, "/v1/users/me/api-tokens", map[string]string{"name": "live"}, callerToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed live: status = %d", resp.StatusCode)
	}
	var live adminApiTokenCreated
	decodeJSON(t, resp, &live)

	resp = h.post(t, "/v1/users/me/api-tokens", map[string]string{"name": "dead"}, callerToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("seed dead: status = %d", resp.StatusCode)
	}
	var dead adminApiTokenCreated
	decodeJSON(t, resp, &dead)

	resp = h.delete(t, "/v1/users/me/api-tokens/"+dead.ID, callerToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("seed revoke status = %d", resp.StatusCode)
	}

	// Default → live only.
	resp = h.get(t, "/v1/users/me/api-tokens", callerToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("default /me list status = %d", resp.StatusCode)
	}
	var def adminApiTokenList
	decodeJSON(t, resp, &def)
	if containsTokenID(def.Data, dead.ID) {
		t.Errorf("/me default list surfaced revoked token %q (variant B drift)", dead.ID)
	}
	if !containsTokenID(def.Data, live.ID) {
		t.Errorf("/me default list missing live token %q", live.ID)
	}

	// include_revoked=true → both.
	resp = h.get(t, "/v1/users/me/api-tokens?include_revoked=true", callerToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/me include_revoked=true status = %d", resp.StatusCode)
	}
	var all adminApiTokenList
	decodeJSON(t, resp, &all)
	if !containsTokenID(all.Data, dead.ID) {
		t.Errorf("/me include_revoked=true list missing revoked token %q", dead.ID)
	}
}

func containsTokenID(rows []adminApiTokenView, id string) bool {
	for _, v := range rows {
		if v.ID == id {
			return true
		}
	}
	return false
}

// TestE2E_AdminApiTokens_CreateValidation covers all 400 paths: bad
// json, empty name, name >100 chars, expires_at malformed, expires_at
// in the past.
func TestE2E_AdminApiTokens_CreateValidation(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	_, adminToken := loginAsReturningID(t, h, auth.RoleAdmin)
	target, _ := loginAsReturningID(t, h, auth.RoleDeveloper)
	path := "/v1/users/" + target.String() + "/api-tokens"

	pastTime := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	cases := []struct {
		name string
		body map[string]any
	}{
		{name: "empty name", body: map[string]any{"name": ""}},
		{name: "name too long", body: map[string]any{"name": strings.Repeat("x", 101)}},
		{name: "expires_at malformed", body: map[string]any{"name": "ok", "expires_at": "not-a-time"}},
		{name: "expires_at in past", body: map[string]any{"name": "ok", "expires_at": pastTime}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.post(t, path, tc.body, adminToken)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			var body response.ErrorBody
			decodeJSON(t, resp, &body)
			if body.Error.Code != response.CodeValidationFailed {
				t.Errorf("code = %q, want %q", body.Error.Code, response.CodeValidationFailed)
			}
		})
	}
}

// TestE2E_AdminApiTokens_CreateExpiresAtFuture sets a future expiry and
// confirms it round-trips through the API.
func TestE2E_AdminApiTokens_CreateExpiresAtFuture(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	_, adminToken := loginAsReturningID(t, h, auth.RoleAdmin)
	target, _ := loginAsReturningID(t, h, auth.RoleDeveloper)
	future := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)

	resp := h.post(t, "/v1/users/"+target.String()+"/api-tokens",
		map[string]any{"name": "scoped", "expires_at": future}, adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var view struct {
		ExpiresAt string `json:"expires_at"`
	}
	decodeJSON(t, resp, &view)
	if view.ExpiresAt == "" {
		t.Errorf("expires_at = empty, want non-empty round-trip")
	}
}

// TestE2E_AdminApiTokens_PostIdempotencyReplayAndMismatch checks that
// the standard Idempotency-Key middleware behavior covers admin-create:
// same key + same body replays the original response (same id, same
// plaintext); same key + different body returns 409
// idempotency_key_mismatch.
func TestE2E_AdminApiTokens_PostIdempotencyReplayAndMismatch(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	_, adminToken := loginAsReturningID(t, h, auth.RoleAdmin)
	target, _ := loginAsReturningID(t, h, auth.RoleDeveloper)
	path := "/v1/users/" + target.String() + "/api-tokens"
	idem := "create-" + uuid.NewString()

	resp := h.postIdem(t, path, map[string]string{"name": "first"}, adminToken, idem)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", resp.StatusCode)
	}
	var first adminApiTokenCreated
	decodeJSON(t, resp, &first)

	// Replay — same key + same body — must return the same token id and plaintext.
	resp = h.postIdem(t, path, map[string]string{"name": "first"}, adminToken, idem)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("replay status = %d, want 201", resp.StatusCode)
	}
	var replay adminApiTokenCreated
	decodeJSON(t, resp, &replay)
	if replay.ID != first.ID || replay.Token != first.Token {
		t.Errorf("replay differs: id (%q vs %q) or token (%q vs %q)",
			replay.ID, first.ID, replay.Token, first.Token)
	}

	// Mismatch — same key + different body — 409.
	resp = h.postIdem(t, path, map[string]string{"name": "different"}, adminToken, idem)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("mismatch status = %d, want 409", resp.StatusCode)
	}
	var body response.ErrorBody
	decodeJSON(t, resp, &body)
	if body.Error.Code != response.CodeIdempotencyMismatch {
		t.Errorf("mismatch code = %q, want %q", body.Error.Code, response.CodeIdempotencyMismatch)
	}
}

// TestE2E_AdminApiTokens_DeleteIdempotencyReplay verifies that DELETE
// replays through the idempotency middleware return the same 204
// status without re-running the (already idempotent) revoke.
func TestE2E_AdminApiTokens_DeleteIdempotencyReplay(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	_, adminToken := loginAsReturningID(t, h, auth.RoleAdmin)
	target, _ := loginAsReturningID(t, h, auth.RoleDeveloper)
	created := adminCreateTokenFor(t, h, adminToken, target, "for-delete")
	idem := "del-" + uuid.NewString()

	resp := h.deleteIdem(t, "/v1/users/"+target.String()+"/api-tokens/"+created.ID, adminToken, idem)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first delete status = %d, want 204", resp.StatusCode)
	}
	resp = h.deleteIdem(t, "/v1/users/"+target.String()+"/api-tokens/"+created.ID, adminToken, idem)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("replay delete status = %d, want 204", resp.StatusCode)
	}
}

// TestE2E_AdminApiTokens_PlaintextOnceOnly confirms the plaintext is
// present on POST and absent in subsequent listings — the prefix is
// surfaced for visual identification, not the full token.
func TestE2E_AdminApiTokens_PlaintextOnceOnly(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	_, adminToken := loginAsReturningID(t, h, auth.RoleAdmin)
	target, _ := loginAsReturningID(t, h, auth.RoleDeveloper)

	created := adminCreateTokenFor(t, h, adminToken, target, "once")
	if created.Token == "" {
		t.Fatal("plaintext token absent from create response")
	}

	resp := h.get(t, "/v1/users/"+target.String()+"/api-tokens", adminToken)
	var list adminApiTokenList
	decodeJSON(t, resp, &list)
	for _, v := range list.Data {
		if v.Token != "" {
			t.Errorf("list leaked plaintext token for id %s: %q", v.ID, v.Token)
		}
		if v.Prefix == "" {
			t.Errorf("list missing prefix for id %s", v.ID)
		}
	}
}
