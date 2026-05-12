// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

// patch sends a PATCH request with an optional bearer and idempotency
// key. Used by users e2e and idempotency e2e tests.
func (h *e2eHarness) patch(t *testing.T, path string, body any, bearer, idemKey string) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPatch, h.srv.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if idemKey != "" {
		req.Header.Set(middleware.HeaderIdempotencyKey, idemKey)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// postIdem is post() with an Idempotency-Key header. The plain post
// helper in auth_e2e_test.go does not accept that header, so the
// idempotency e2e tests use this one.
func (h *e2eHarness) postIdem(t *testing.T, path string, body any, bearer, idemKey string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, h.srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if idemKey != "" {
		req.Header.Set(middleware.HeaderIdempotencyKey, idemKey)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// deleteIdem is delete() with an Idempotency-Key header — used by the
// admin api-tokens idempotency replay test.
func (h *e2eHarness) deleteIdem(t *testing.T, path, bearer, idemKey string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodDelete, h.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if idemKey != "" {
		req.Header.Set(middleware.HeaderIdempotencyKey, idemKey)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// uniqueEmail produces an email that won't collide with concurrent or
// previously-run tests sharing the same Postgres container.
func uniqueEmail(prefix string) string {
	return fmt.Sprintf("%s-%s@e2e.test", prefix, uuid.NewString())
}

// userBody is the JSON payload accepted by POST /v1/users in tests.
func userBody(email, password, role string) map[string]any {
	return map[string]any{
		"email":        email,
		"password":     password,
		"display_name": "",
		"role":         role,
	}
}

func TestE2E_Users_CreateRequiresAdmin(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	devToken := loginAs(t, h, auth.RoleDeveloper)
	resp := h.post(t, "/v1/users", userBody(uniqueEmail("nope"), "valid-password-12", "developer"), devToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	var body response.ErrorBody
	decodeJSON(t, resp, &body)
	if body.Error.Code != response.CodePermissionDenied {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodePermissionDenied)
	}
}

func TestE2E_Users_CreateAdminHappyPath(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	email := uniqueEmail("created")

	resp := h.post(t, "/v1/users", userBody(email, "secret-password-12", "operator"), adminToken)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}

	var view struct {
		ID       string  `json:"id"`
		Email    string  `json:"email"`
		Role     string  `json:"role"`
		Password *string `json:"password"`      // must be absent
		PassHash *string `json:"password_hash"` // must be absent
	}
	decodeJSON(t, resp, &view)
	if view.Email != email || view.Role != "operator" {
		t.Errorf("view = %+v, want email=%q role=operator", view, email)
	}
	if view.Password != nil || view.PassHash != nil {
		t.Errorf("response leaked credential field: %+v", view)
	}
	if _, err := uuid.Parse(view.ID); err != nil {
		t.Errorf("id = %q, not a uuid", view.ID)
	}
}

func TestE2E_Users_CreateValidation(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)

	tests := []struct {
		name string
		body map[string]any
	}{
		{name: "invalid email", body: userBody("not-an-email", "valid-password-12", "developer")},
		{name: "short password", body: userBody(uniqueEmail("v"), "short", "developer")},
		{name: "invalid role", body: userBody(uniqueEmail("v"), "valid-password-12", "godking")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.post(t, "/v1/users", tc.body, adminToken)
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

func TestE2E_Users_CreateDuplicateEmail(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	email := uniqueEmail("dup")

	resp1 := h.post(t, "/v1/users", userBody(email, "valid-password-12", "developer"), adminToken)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", resp1.StatusCode)
	}

	resp2 := h.post(t, "/v1/users", userBody(email, "different-password-1", "viewer"), adminToken)
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("duplicate status = %d, want 409", resp2.StatusCode)
	}
	var body response.ErrorBody
	decodeJSON(t, resp2, &body)
	if body.Error.Code != response.CodeConflict {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodeConflict)
	}
}

func TestE2E_Users_GetReadGate(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	devID, _, _ := seedUserWithRole(t, context.Background(), h.store, auth.RoleDeveloper)

	resp := h.get(t, "/v1/users/"+devID.String(), adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("admin GET status = %d, want 200", resp.StatusCode)
	}

	devToken := loginAs(t, h, auth.RoleDeveloper)
	resp2 := h.get(t, "/v1/users/"+devID.String(), devToken)
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("developer GET status = %d, want 403 (no user:read)", resp2.StatusCode)
	}
}

func TestE2E_Users_List(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.get(t, "/v1/users?limit=5", adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var list struct {
		Data []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"data"`
		Meta struct {
			NextCursor *string `json:"next_cursor"`
		} `json:"meta"`
	}
	decodeJSON(t, resp, &list)
	if len(list.Data) == 0 {
		t.Errorf("data is empty, want at least one user")
	}
}

func TestE2E_Users_ListByEmail(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	_, email, _ := seedUserWithRole(t, context.Background(), h.store, auth.RoleViewer)

	resp := h.get(t, "/v1/users?email="+email, adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var list struct {
		Data []struct {
			Email string `json:"email"`
		} `json:"data"`
	}
	decodeJSON(t, resp, &list)
	if len(list.Data) != 1 || list.Data[0].Email != email {
		t.Errorf("list = %+v, want exactly [%s]", list.Data, email)
	}
}

func TestE2E_Users_PatchMe_DisplayNameAndPassword(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	_, email, _ := seedUserWithRole(t, context.Background(), h.store, auth.RoleDeveloper)

	loginResp := h.post(t, "/v1/auth/login", map[string]string{
		"email": email, "password": "correct-horse-battery-staple",
	}, "")
	var login struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, loginResp, &login)

	resp := h.patch(t, "/v1/users/me", map[string]any{"display_name": "Alice"}, login.AccessToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("display_name patch status = %d, want 200", resp.StatusCode)
	}
	var view struct {
		DisplayName string `json:"display_name"`
	}
	decodeJSON(t, resp, &view)
	if view.DisplayName != "Alice" {
		t.Errorf("display_name = %q, want Alice", view.DisplayName)
	}

	resp2 := h.patch(t, "/v1/users/me", map[string]any{"password": "brand-new-password-99"}, login.AccessToken, "")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("password patch status = %d, want 200", resp2.StatusCode)
	}

	loginNew := h.post(t, "/v1/auth/login", map[string]string{
		"email": email, "password": "brand-new-password-99",
	}, "")
	if loginNew.StatusCode != http.StatusOK {
		t.Errorf("login with new password status = %d, want 200", loginNew.StatusCode)
	}
	loginOld := h.post(t, "/v1/auth/login", map[string]string{
		"email": email, "password": "correct-horse-battery-staple",
	}, "")
	if loginOld.StatusCode != http.StatusUnauthorized {
		t.Errorf("login with old password status = %d, want 401", loginOld.StatusCode)
	}
}

func TestE2E_Users_PatchMe_RoleForbidden(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	devToken := loginAs(t, h, auth.RoleDeveloper)
	resp := h.patch(t, "/v1/users/me", map[string]any{"role": "admin"}, devToken, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	var body response.ErrorBody
	decodeJSON(t, resp, &body)
	if body.Error.Code != response.CodePermissionDenied {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodePermissionDenied)
	}
	fields, _ := body.Error.Details["forbidden_fields"].([]any)
	if len(fields) != 1 || fields[0] != "role" {
		t.Errorf("forbidden_fields = %v, want [role]", fields)
	}
}

func TestE2E_Users_AdminPatchOtherRole(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	devID, _, _ := seedUserWithRole(t, context.Background(), h.store, auth.RoleDeveloper)

	resp := h.patch(t, "/v1/users/"+devID.String(),
		map[string]any{"role": "operator"}, adminToken, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var view struct {
		Role string `json:"role"`
	}
	decodeJSON(t, resp, &view)
	if view.Role != "operator" {
		t.Errorf("role = %q, want operator", view.Role)
	}
}

func TestE2E_Users_AdminCannotChangeOwnRole(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminID, email, _ := seedUserWithRole(t, context.Background(), h.store, auth.RoleAdmin)
	loginResp := h.post(t, "/v1/auth/login", map[string]string{
		"email": email, "password": "correct-horse-battery-staple",
	}, "")
	var login struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, loginResp, &login)

	resp := h.patch(t, "/v1/users/"+adminID.String(),
		map[string]any{"role": "operator"}, login.AccessToken, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	var body response.ErrorBody
	decodeJSON(t, resp, &body)
	fields, _ := body.Error.Details["forbidden_fields"].([]any)
	if len(fields) != 1 || fields[0] != "role" {
		t.Errorf("forbidden_fields = %v, want [role]", fields)
	}
}

func TestE2E_Users_PatchOtherRequiresAdmin(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	devToken := loginAs(t, h, auth.RoleDeveloper)
	other, _, _ := seedUserWithRole(t, context.Background(), h.store, auth.RoleViewer)

	resp := h.patch(t, "/v1/users/"+other.String(),
		map[string]any{"display_name": "hijack"}, devToken, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestE2E_Users_DeleteSelfRejected(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminID, email, _ := seedUserWithRole(t, context.Background(), h.store, auth.RoleAdmin)
	loginResp := h.post(t, "/v1/auth/login", map[string]string{
		"email": email, "password": "correct-horse-battery-staple",
	}, "")
	var login struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, loginResp, &login)

	resp := h.delete(t, "/v1/users/"+adminID.String(), login.AccessToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestE2E_Users_DeleteUnknown404(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	resp := h.delete(t, "/v1/users/"+uuid.NewString(), adminToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestE2E_Users_DeleteHappyPath(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	target, _, _ := seedUserWithRole(t, context.Background(), h.store, auth.RoleViewer)

	resp := h.delete(t, "/v1/users/"+target.String(), adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}

	resp2 := h.get(t, "/v1/users/"+target.String(), adminToken)
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("post-delete GET status = %d, want 404", resp2.StatusCode)
	}
}

func TestE2E_Users_DeleteEmailReuseAfterSoftDelete(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	target, email, _ := seedUserWithRole(t, context.Background(), h.store, auth.RoleViewer)

	resp := h.delete(t, "/v1/users/"+target.String(), adminToken)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}

	// Partial unique index on lower(email) WHERE deleted_at IS NULL means
	// the address is reusable as soon as the row is soft-deleted.
	resp2 := h.post(t, "/v1/users", userBody(email, "fresh-password-12", "developer"), adminToken)
	if resp2.StatusCode != http.StatusCreated {
		t.Errorf("reuse status = %d, want 201", resp2.StatusCode)
	}
}

func TestE2E_Users_DeleteRequiresAdmin(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	devToken := loginAs(t, h, auth.RoleDeveloper)
	target, _, _ := seedUserWithRole(t, context.Background(), h.store, auth.RoleViewer)

	resp := h.delete(t, "/v1/users/"+target.String(), devToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestE2E_Idempotency_ReplayOnRepeat(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	email := uniqueEmail("idem-replay")
	body := userBody(email, "good-password-aaaaaa", "developer")
	key := "test-key-" + uuid.NewString()

	resp1 := h.postIdem(t, "/v1/users", body, adminToken, key)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d", resp1.StatusCode)
	}
	var first map[string]any
	decodeJSON(t, resp1, &first)

	resp2 := h.postIdem(t, "/v1/users", body, adminToken, key)
	if resp2.StatusCode != http.StatusCreated {
		t.Errorf("replay status = %d, want 201", resp2.StatusCode)
	}
	var second map[string]any
	decodeJSON(t, resp2, &second)
	if first["id"] != second["id"] {
		t.Errorf("replay id %v != original %v", second["id"], first["id"])
	}
}

func TestE2E_Idempotency_BodyMismatch(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	key := "test-key-" + uuid.NewString()

	resp1 := h.postIdem(t, "/v1/users",
		userBody(uniqueEmail("idem-mismatch-1"), "good-password-aaaaaa", "developer"),
		adminToken, key)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d", resp1.StatusCode)
	}

	resp2 := h.postIdem(t, "/v1/users",
		userBody(uniqueEmail("idem-mismatch-2"), "good-password-aaaaaa", "developer"),
		adminToken, key)
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp2.StatusCode)
	}
	var body response.ErrorBody
	decodeJSON(t, resp2, &body)
	if body.Error.Code != response.CodeIdempotencyMismatch {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodeIdempotencyMismatch)
	}
}

func TestE2E_Idempotency_ErrorNotCached(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	key := "test-key-" + uuid.NewString()

	resp1 := h.postIdem(t, "/v1/users",
		userBody("not-an-email", "good-password-aaaaaa", "developer"),
		adminToken, key)
	if resp1.StatusCode != http.StatusBadRequest {
		t.Fatalf("first status = %d, want 400", resp1.StatusCode)
	}

	good := userBody(uniqueEmail("idem-recover"), "good-password-aaaaaa", "developer")
	resp2 := h.postIdem(t, "/v1/users", good, adminToken, key)
	if resp2.StatusCode != http.StatusCreated {
		t.Errorf("retry-after-error status = %d, want 201 (4xx must not be cached)", resp2.StatusCode)
	}
}

func TestE2E_Idempotency_LoginNotIntercepted(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	_, email, pw := seedUserWithRole(t, context.Background(), h.store, auth.RoleDeveloper)

	body := map[string]string{"email": email, "password": pw}
	key := "should-be-ignored"

	resp := h.postIdem(t, "/v1/auth/login", body, "", key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first login status = %d, want 200", resp.StatusCode)
	}
	var first struct {
		RefreshToken string `json:"refresh_token"`
	}
	decodeJSON(t, resp, &first)

	resp2 := h.postIdem(t, "/v1/auth/login", body, "", key)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second login status = %d, want 200", resp2.StatusCode)
	}
	var second struct {
		RefreshToken string `json:"refresh_token"`
	}
	decodeJSON(t, resp2, &second)

	if first.RefreshToken == "" || second.RefreshToken == "" {
		t.Fatalf("missing refresh tokens: %q / %q", first.RefreshToken, second.RefreshToken)
	}
	if first.RefreshToken == second.RefreshToken {
		t.Errorf("idempotency replayed login — refresh tokens identical, login carve-out broken")
	}
}

func TestE2E_Idempotency_KeyTooLong(t *testing.T) {
	h := newE2E(t)
	defer h.close()

	adminToken := loginAs(t, h, auth.RoleAdmin)
	body := userBody(uniqueEmail("idem-long"), "good-password-aaaaaa", "developer")
	key := strings.Repeat("k", middleware.IdempotencyKeyMaxLength+1)

	resp := h.postIdem(t, "/v1/users", body, adminToken, key)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
