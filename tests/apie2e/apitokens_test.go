// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"net/http"
	"testing"

	"github.com/otherix/otherix/internal/api/middleware"
	"github.com/otherix/otherix/internal/auth"
)

// apiTokenCreateView decodes the once-only api-token create response.
type apiTokenCreateView struct {
	ID     string `json:"id"`
	Prefix string `json:"prefix"`
	Token  string `json:"token"`
}

// TestAPITokenCreateCarvedOutOfIdempotency drives the real router seam: the
// api-token create routes must NOT run under the Idempotency middleware, so
// the once-only otx_ plaintext is never persisted into the idempotency_keys
// store (a 24h at-rest cache of a live bearer credential). The observable
// contract, mirroring the join-token carve-out, is that two POSTs with the
// SAME Idempotency-Key mint two DIFFERENT tokens rather than replaying the
// first cached plaintext. Under idem this replays (identical id + token); with
// the carve-out each call mints fresh.
func TestAPITokenCreateCarvedOutOfIdempotency(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	key := "idem-apitoken-carveout"
	hdr := map[string]string{middleware.HeaderIdempotencyKey: key, "Content-Type": "application/json"}
	body := map[string]any{"name": "ci-token"}

	resp := h.do(t, http.MethodPost, "/v1/users/me/api-tokens", body, admin, hdr)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201", resp.StatusCode)
	}
	var first apiTokenCreateView
	decodeJSON(t, resp, &first)
	if first.Token == "" {
		t.Fatalf("first create returned empty plaintext token")
	}

	// Same Idempotency-Key + same body. If create were under idem this would
	// replay the first response verbatim (same id + same plaintext, cached at
	// rest). Carved out, it mints a brand-new token.
	resp = h.do(t, http.MethodPost, "/v1/users/me/api-tokens", body, admin, hdr)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("second create status = %d, want 201 (fresh, not idem replay)", resp.StatusCode)
	}
	var second apiTokenCreateView
	decodeJSON(t, resp, &second)

	if second.ID == first.ID || second.Token == first.Token {
		t.Errorf("api-token create replayed under idempotency (id/token identical) - plaintext is cached at rest: first id=%q second id=%q", first.ID, second.ID)
	}
}
