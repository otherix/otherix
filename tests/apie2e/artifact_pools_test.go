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

func TestArtifactPoolCRUD(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	resp := h.post(t, "/v1/artifact-pools", map[string]any{
		"name":               "gold",
		"replication_factor": 3,
		"membership":         map[string]any{"all_nodes": true},
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var view struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		ReplicationFactor any    `json:"replication_factor"`
	}
	decodeJSON(t, resp, &view)
	if view.Name != "gold" {
		t.Errorf("name = %q, want gold", view.Name)
	}
	if f, _ := view.ReplicationFactor.(float64); f != 3 {
		t.Errorf("replication_factor = %v, want 3", view.ReplicationFactor)
	}

	resp = h.post(t, "/v1/artifact-pools", map[string]any{
		"name": "bronze", "replication_factor": "all",
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("all create status = %d, want 201", resp.StatusCode)
	}
	var allView struct {
		ReplicationFactor any `json:"replication_factor"`
	}
	decodeJSON(t, resp, &allView)
	if s, _ := allView.ReplicationFactor.(string); s != "all" {
		t.Errorf("replication_factor = %v, want \"all\"", allView.ReplicationFactor)
	}

	// Omitted replication_factor defaults to 1 (OpenAPI default: 1).
	resp = h.post(t, "/v1/artifact-pools", map[string]any{"name": "silver"}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("omitted-rf create status = %d, want 201", resp.StatusCode)
	}
	var silver struct {
		ReplicationFactor any `json:"replication_factor"`
	}
	decodeJSON(t, resp, &silver)
	if f, _ := silver.ReplicationFactor.(float64); f != 1 {
		t.Errorf("omitted replication_factor = %v, want 1", silver.ReplicationFactor)
	}

	resp = h.post(t, "/v1/artifact-pools", map[string]any{"name": "bad", "replication_factor": 0}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("zero-K status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.get(t, "/v1/artifact-pools/gold", admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get by name status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.get(t, "/v1/artifact-pools", admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.delete(t, "/v1/artifact-pools/gold", admin)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestArtifactPoolUpdate(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	developer, _ := loginAs(t, h, auth.RoleDeveloper)

	resp := h.post(t, "/v1/artifact-pools", map[string]any{
		"name": "gold", "replication_factor": 1,
		"membership": map[string]any{"all_nodes": true},
	}, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	// Increase replication_factor: 200, returned view shows 2.
	resp = h.patch(t, "/v1/artifact-pools/gold", map[string]any{"replication_factor": 2}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch rf status = %d, want 200", resp.StatusCode)
	}
	var patched struct {
		ReplicationFactor any `json:"replication_factor"`
	}
	decodeJSON(t, resp, &patched)
	if f, _ := patched.ReplicationFactor.(float64); f != 2 {
		t.Errorf("patched replication_factor = %v, want 2", patched.ReplicationFactor)
	}

	// A subsequent GET reflects the new value.
	resp = h.get(t, "/v1/artifact-pools/gold", admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		ReplicationFactor any `json:"replication_factor"`
	}
	decodeJSON(t, resp, &got)
	if f, _ := got.ReplicationFactor.(float64); f != 2 {
		t.Errorf("get replication_factor = %v, want 2", got.ReplicationFactor)
	}

	// Explicit 2-node membership with replication_factor=3 exceeds members -> 400.
	resp = h.patch(t, "/v1/artifact-pools/gold", map[string]any{
		"replication_factor": 3,
		"membership":         map[string]any{"all_nodes": false, "nodes": []string{"n1", "n2"}},
	}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("rf>members status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown id -> 404.
	resp = h.patch(t, "/v1/artifact-pools/does-not-exist", map[string]any{"replication_factor": 2}, admin)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown-id status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// Non-manage role -> 403.
	resp = h.patch(t, "/v1/artifact-pools/gold", map[string]any{"replication_factor": 2}, developer)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("developer patch status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// Empty body (no fields) -> 400.
	resp = h.patch(t, "/v1/artifact-pools/gold", map[string]any{}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty-body status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}
