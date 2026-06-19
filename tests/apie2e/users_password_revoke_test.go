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

// TestPasswordChangeRevokesOwnRefreshTokens drives the real seam for audit
// M7: Login issues a refresh token, PATCH /v1/users/me changes the password,
// and the pre-change refresh token must no longer rotate. Without the revoke
// a stolen 30-day refresh token would keep minting access tokens after the
// victim rotated their password.
func TestPasswordChangeRevokesOwnRefreshTokens(t *testing.T) {
	h := newE2E(t)
	_, email, pw := seedUser(t, h.store, auth.RoleDeveloper)

	resp := h.post(t, "/v1/auth/login", map[string]string{"email": email, "password": pw}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	var pair tokenPair
	decodeJSON(t, resp, &pair)

	// Change own password. The access JWT is stateless and still valid
	// for its short TTL, so it authenticates this very request.
	resp = h.patch(t, "/v1/users/me", map[string]string{"password": "rotated-password-123"}, pair.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch /v1/users/me status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// The pre-change refresh token must be dead.
	resp = h.post(t, "/v1/auth/refresh", map[string]string{"refresh_token": pair.RefreshToken}, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("refresh with pre-change token status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// The new password logs in and starts a fresh session.
	resp = h.post(t, "/v1/auth/login", map[string]string{"email": email, "password": "rotated-password-123"}, "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("login with new password status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestAdminPasswordResetRevokesTargetRefreshTokens covers the admin half of
// password-change refresh revocation: PATCH /v1/users/{id} with a new password
// must revoke the TARGET user's refresh tokens, not the admin's.
func TestAdminPasswordResetRevokesTargetRefreshTokens(t *testing.T) {
	h := newE2E(t)
	victimID, email, pw := seedUser(t, h.store, auth.RoleDeveloper)

	resp := h.post(t, "/v1/auth/login", map[string]string{"email": email, "password": pw}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("victim login status = %d, want 200", resp.StatusCode)
	}
	var victim tokenPair
	decodeJSON(t, resp, &victim)

	admin, _ := loginAs(t, h, auth.RoleAdmin)
	resp = h.patch(t, "/v1/users/"+victimID.String(), map[string]string{"password": "admin-reset-pass-123"}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin patch /v1/users/{id} status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// The victim's pre-reset refresh token must be dead.
	resp = h.post(t, "/v1/auth/refresh", map[string]string{"refresh_token": victim.RefreshToken}, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("victim refresh after admin reset status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}
