// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"net/http"
	"testing"

	"github.com/otherix/otherix/internal/auth"
)

// TestUserRoleChangeRevokesRefreshTokens drives the real seam for role-change
// refresh revocation: PATCH /v1/users/{id} with a different role must revoke the TARGET user's
// refresh tokens, mirroring the password-change path. A stale or stolen refresh
// token must not keep minting access tokens after the operator changed the
// user's standing (e.g. a demotion or lockdown).
func TestUserRoleChangeRevokesRefreshTokens(t *testing.T) {
	h := newE2E(t)
	victimID, email, pw := seedUser(t, h.store, auth.RoleDeveloper)

	resp := h.post(t, "/v1/auth/login", map[string]string{"email": email, "password": pw}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("victim login status = %d, want 200", resp.StatusCode)
	}
	var victim tokenPair
	decodeJSON(t, resp, &victim)

	admin, _ := loginAs(t, h, auth.RoleAdmin)
	resp = h.patch(t, "/v1/users/"+victimID.String(), map[string]string{"role": string(auth.RoleViewer)}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin patch role status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// The victim's pre-change refresh token must be dead.
	resp = h.post(t, "/v1/auth/refresh", map[string]string{"refresh_token": victim.RefreshToken}, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("victim refresh after role change status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestUserDisplayNameChangeKeepsRefreshTokens guards against over-revocation:
// a PATCH that changes neither password nor role (display_name only) must leave
// the user's refresh tokens intact.
func TestUserDisplayNameChangeKeepsRefreshTokens(t *testing.T) {
	h := newE2E(t)
	victimID, email, pw := seedUser(t, h.store, auth.RoleDeveloper)

	resp := h.post(t, "/v1/auth/login", map[string]string{"email": email, "password": pw}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("victim login status = %d, want 200", resp.StatusCode)
	}
	var victim tokenPair
	decodeJSON(t, resp, &victim)

	admin, _ := loginAs(t, h, auth.RoleAdmin)
	resp = h.patch(t, "/v1/users/"+victimID.String(), map[string]string{"display_name": "Renamed"}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin patch display_name status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// The victim's refresh token must still rotate.
	resp = h.post(t, "/v1/auth/refresh", map[string]string{"refresh_token": victim.RefreshToken}, "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("victim refresh after display_name change status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}
