// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package api_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/otherix/otherix/internal/auth"
)

// loginAs seeds a user with role and returns its access token.
func loginAs(t *testing.T, h *e2eHarness, role auth.Role) string {
	t.Helper()
	_, email, pw := seedUserWithRole(t, context.Background(), h.store, role)
	resp := h.post(t, "/v1/auth/login", map[string]string{"email": email, "password": pw}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login(%s) status = %d, want 200", role, resp.StatusCode)
	}
	var login struct {
		AccessToken string `json:"access_token"`
	}
	decodeJSON(t, resp, &login)
	return login.AccessToken
}

// TestE2E_RBAC_APITokensAllRoles asserts that every role can exercise
// the api-tokens self-service endpoints — docs/rbac.md grants
// `api_token:manage:own` to all four roles, so the middleware admits
// each one. Denied paths are covered by the unit tests in
// internal/api/middleware/permission_test.go.
func TestE2E_RBAC_APITokensAllRoles(t *testing.T) {
	h := newE2E(t)

	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			access := loginAs(t, h, role)

			resp := h.post(t, "/v1/users/me/api-tokens",
				map[string]string{"name": "rbac-" + string(role)}, access)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("POST api-tokens (%s) status = %d, want 201", role, resp.StatusCode)
			}

			resp = h.get(t, "/v1/users/me/api-tokens", access)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET api-tokens (%s) status = %d, want 200", role, resp.StatusCode)
			}
		})
	}
}
