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

func TestClusterDefaultArtifactPool(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	// Unset -> 404.
	resp := h.get(t, "/v1/cluster/default-artifact-pool", admin)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get-unset status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// PUT a name that is not an artifact pool -> 400.
	resp = h.put(t, "/v1/cluster/default-artifact-pool", map[string]any{"name": "nope"}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("put-unknown status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Create an artifact pool, then PUT it as default -> 200.
	h.post(t, "/v1/artifact-pools", map[string]any{"name": "gold", "replication_factor": 1}, admin).Body.Close()
	resp = h.put(t, "/v1/cluster/default-artifact-pool", map[string]any{"name": "gold"}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// GET now returns it.
	resp = h.get(t, "/v1/cluster/default-artifact-pool", admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// DELETE -> 204, then GET -> 404.
	h.delete(t, "/v1/cluster/default-artifact-pool", admin).Body.Close()
	resp = h.get(t, "/v1/cluster/default-artifact-pool", admin)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get-after-clear status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}
