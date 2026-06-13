// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"net/http"
	"testing"

	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
)

func TestIdempotencyReplaySameKeyAndBody(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	body := newNetworkBody()
	key := "idem-" + body["name"].(string)
	hdr := map[string]string{middleware.HeaderIdempotencyKey: key, "Content-Type": "application/json"}

	resp := h.do(t, http.MethodPost, "/v1/networks", body, admin, hdr)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", resp.StatusCode)
	}
	var first networkView
	decodeJSON(t, resp, &first)

	// Replay: same key + same body -> cached response verbatim, no second row.
	resp = h.do(t, http.MethodPost, "/v1/networks", body, admin, hdr)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("replay status = %d, want 201 (cached)", resp.StatusCode)
	}
	var replay networkView
	decodeJSON(t, resp, &replay)
	if replay.ID != first.ID {
		t.Errorf("replay id = %q, want cached %q", replay.ID, first.ID)
	}

	// Exactly one network exists.
	resp = h.get(t, "/v1/networks", admin)
	var list struct {
		Data []networkView `json:"data"`
	}
	decodeJSON(t, resp, &list)
	count := 0
	for _, n := range list.Data {
		if n.Name == body["name"] {
			count++
		}
	}
	if count != 1 {
		t.Errorf("network count = %d, want 1 (replay must not create a second)", count)
	}
}

// TestIdempotencyPerUserScopeNoCrossUserCollision drives the real seam for
// audit R2-L10: two different users posting the same mutating endpoint with the
// SAME Idempotency-Key but DIFFERENT bodies must both succeed. Idempotency keys
// are scoped per user, so neither user can squat a key string to grief the
// other (a 24h 409 denial under the old global-key scheme).
func TestIdempotencyPerUserScopeNoCrossUserCollision(t *testing.T) {
	h := newE2E(t)
	adminA, idA := loginAs(t, h, auth.RoleAdmin)
	adminB, idB := loginAs(t, h, auth.RoleAdmin)
	if idA == idB {
		t.Fatalf("loginAs returned the same user id %q for both admins, want distinct", idA)
	}

	key := "idem-shared-key"
	hdr := map[string]string{middleware.HeaderIdempotencyKey: key, "Content-Type": "application/json"}

	resp := h.do(t, http.MethodPost, "/v1/networks", newNetworkBody(), adminA, hdr)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("user A create status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	// User B uses the SAME key with a DIFFERENT body. Per-user scope means no
	// cross-user collision: B must create its own network, not get 409.
	resp = h.do(t, http.MethodPost, "/v1/networks", newNetworkBody(), adminB, hdr)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("user B create with shared key status = %d, want 201 (no cross-user collision)", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestIdempotencyMismatchSameKeyDifferentBody(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	key := "idem-mismatch-key"
	hdr := map[string]string{middleware.HeaderIdempotencyKey: key, "Content-Type": "application/json"}

	resp := h.do(t, http.MethodPost, "/v1/networks", newNetworkBody(), admin, hdr)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	// Same key, different body -> 409 idempotency_key_mismatch.
	resp = h.do(t, http.MethodPost, "/v1/networks", newNetworkBody(), admin, hdr)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("mismatch status = %d, want 409", resp.StatusCode)
	}
	var body response.ErrorBody
	decodeJSON(t, resp, &body)
	if body.Error.Code != response.CodeIdempotencyMismatch {
		t.Errorf("code = %q, want %q", body.Error.Code, response.CodeIdempotencyMismatch)
	}
}
