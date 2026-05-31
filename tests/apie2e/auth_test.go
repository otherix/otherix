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

type tokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

func TestLoginRefreshLogoutRotation(t *testing.T) {
	h := newE2E(t)
	_, email, pw := seedUser(t, h.store, auth.RoleDeveloper)

	// Login.
	resp := h.post(t, "/v1/auth/login", map[string]string{"email": email, "password": pw}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	var first tokenPair
	decodeJSON(t, resp, &first)
	if first.AccessToken == "" || first.RefreshToken == "" {
		t.Fatalf("login returned empty tokens: %+v", first)
	}

	// Refresh rotates the refresh token (new child, parent revoked).
	resp = h.post(t, "/v1/auth/refresh", map[string]string{"refresh_token": first.RefreshToken}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200", resp.StatusCode)
	}
	var second tokenPair
	decodeJSON(t, resp, &second)
	if second.RefreshToken == "" || second.RefreshToken == first.RefreshToken {
		t.Fatalf("refresh did not rotate the token: first=%q second=%q", first.RefreshToken, second.RefreshToken)
	}

	// Logout invalidates the current (child) refresh token.
	resp = h.post(t, "/v1/auth/logout", map[string]string{"refresh_token": second.RefreshToken}, second.AccessToken)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 200/204", resp.StatusCode)
	}
	resp.Body.Close()

	// The logged-out token no longer refreshes.
	resp = h.post(t, "/v1/auth/refresh", map[string]string{"refresh_token": second.RefreshToken}, "")
	if resp.StatusCode == http.StatusOK {
		t.Errorf("refresh after logout = 200, want rejected")
	}
	resp.Body.Close()
}

func TestRefreshReplayRevokesFamily(t *testing.T) {
	h := newE2E(t)
	_, email, pw := seedUser(t, h.store, auth.RoleDeveloper)

	resp := h.post(t, "/v1/auth/login", map[string]string{"email": email, "password": pw}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	var first tokenPair
	decodeJSON(t, resp, &first)

	// Rotate once: parent revoked, child issued.
	resp = h.post(t, "/v1/auth/refresh", map[string]string{"refresh_token": first.RefreshToken}, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200", resp.StatusCode)
	}
	var child tokenPair
	decodeJSON(t, resp, &child)

	// Replay the already-revoked PARENT: theft signal. Family is burned.
	resp = h.post(t, "/v1/auth/refresh", map[string]string{"refresh_token": first.RefreshToken}, "")
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("replay of revoked parent = 200, want rejected")
	}
	resp.Body.Close()

	// The previously-valid child is now revoked too (whole family burned).
	resp = h.post(t, "/v1/auth/refresh", map[string]string{"refresh_token": child.RefreshToken}, "")
	if resp.StatusCode == http.StatusOK {
		t.Errorf("child refresh after family revocation = 200, want rejected")
	}
	resp.Body.Close()
}

func TestLoginWrongPassword(t *testing.T) {
	h := newE2E(t)
	_, email, _ := seedUser(t, h.store, auth.RoleDeveloper)
	resp := h.post(t, "/v1/auth/login", map[string]string{"email": email, "password": "wrong-password"}, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login wrong-password status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}
