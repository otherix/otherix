// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
)

type networkView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	BridgeName string `json:"bridge_name"`
	MTU        int32  `json:"mtu"`
}

func newNetworkBody() map[string]any {
	suffix := uuid.NewString()[:8]
	return map[string]any{
		"name":        "net-" + suffix,
		"type":        "bridge",
		"bridge_name": "br" + suffix[:6],
	}
}

func TestNetworksCRUDAsAdmin(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	resp := h.post(t, "/v1/networks", newNetworkBody(), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	var created networkView
	decodeJSON(t, resp, &created)
	if created.ID == "" || created.Type != "bridge" {
		t.Fatalf("create view = %+v", created)
	}
	if created.MTU != 1500 {
		t.Errorf("default mtu = %d, want 1500", created.MTU)
	}

	resp = h.get(t, "/v1/networks/"+created.ID, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	// Patch mtu.
	resp = h.patch(t, "/v1/networks/"+created.ID, map[string]any{"mtu": 9000}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	var patched networkView
	decodeJSON(t, resp, &patched)
	if patched.MTU != 9000 {
		t.Errorf("patched mtu = %d, want 9000", patched.MTU)
	}

	// type is immutable.
	resp = h.patch(t, "/v1/networks/"+created.ID, map[string]any{"type": "vlan"}, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("patch type status = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.delete(t, "/v1/networks/"+created.ID, admin)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 204/200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestNetworksManageForbiddenForViewer(t *testing.T) {
	h := newE2E(t)
	viewer, _ := loginAs(t, h, auth.RoleViewer)
	// Viewer holds network:read but not network:manage.
	resp := h.post(t, "/v1/networks", newNetworkBody(), viewer)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer create status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// But viewer can list.
	resp = h.get(t, "/v1/networks", viewer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer list status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}
