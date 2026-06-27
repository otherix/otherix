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

func TestClusterDefaultNetwork(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	// Unset -> 404 with envelope code default_network_not_set.
	resp := h.get(t, "/v1/cluster/default-network", admin)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get-unset status = %d, want 404", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decodeJSON(t, resp, &env)
	if env.Error.Code != "default_network_not_set" {
		t.Errorf("get-unset code = %q, want default_network_not_set", env.Error.Code)
	}

	// Create a bridge network named qnet.
	netBody := map[string]any{"name": "qnet", "type": "bridge", "bridge_name": "qbr0"}
	resp = h.post(t, "/v1/networks", netBody, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("network create status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	// PUT it as the default -> 200 {"name":"qnet"}.
	resp = h.put(t, "/v1/cluster/default-network", map[string]any{"name": "qnet"}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Name string `json:"name"`
	}
	decodeJSON(t, resp, &got)
	if got.Name != "qnet" {
		t.Errorf("put name = %q, want qnet", got.Name)
	}

	// GET now returns it.
	resp = h.get(t, "/v1/cluster/default-network", admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	got.Name = ""
	decodeJSON(t, resp, &got)
	if got.Name != "qnet" {
		t.Errorf("get name = %q, want qnet", got.Name)
	}

	// PUT an unknown name -> 400 validation_failed.
	resp = h.put(t, "/v1/cluster/default-network", map[string]any{"name": "does-not-exist"}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("put-unknown status = %d, want 400", resp.StatusCode)
	}
	env.Error.Code = ""
	decodeJSON(t, resp, &env)
	if env.Error.Code != "validation_failed" {
		t.Errorf("put-unknown code = %q, want validation_failed", env.Error.Code)
	}

	// DELETE -> 204, then GET -> 404 again.
	resp = h.delete(t, "/v1/cluster/default-network", admin)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
	resp = h.get(t, "/v1/cluster/default-network", admin)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get-after-clear status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// PUT as a non-admin (developer) -> 403 permission_denied.
	dev, _ := loginAs(t, h, auth.RoleDeveloper)
	resp = h.put(t, "/v1/cluster/default-network", map[string]any{"name": "qnet"}, dev)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("put-as-developer status = %d, want 403", resp.StatusCode)
	}
	env.Error.Code = ""
	decodeJSON(t, resp, &env)
	if env.Error.Code != "permission_denied" {
		t.Errorf("put-as-developer code = %q, want permission_denied", env.Error.Code)
	}
}
