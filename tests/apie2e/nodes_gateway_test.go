// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/internal/auth"
)

// nodeGatewayRolesView captures the fields the gateway-toggle assertions read
// from the full Node projection.
type nodeGatewayRolesView struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
}

// TestNodeGatewayEnableDisable drives the real gateway-toggle routes: an admin
// assigns the ingress-gateway role to a hypervisor node (roles gain "gateway"),
// re-enabling is idempotent (200, unchanged), and disabling returns the node to
// hypervisor-only. Assigning the role is node:manage (admin), so an operator
// token is refused with 403.
func TestNodeGatewayEnableDisable(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	operator, _ := loginAs(t, h, auth.RoleOperator)

	// A default node created through the public API is a hypervisor.
	resp := h.post(t, "/v1/nodes", newNodeBody(), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create node status = %d, want 201", resp.StatusCode)
	}
	var created nodeGatewayRolesView
	decodeJSON(t, resp, &created)
	if diff := cmp.Diff([]string{"hypervisor"}, created.Roles); diff != "" {
		t.Fatalf("fresh node roles mismatch (-want +got):\n%s", diff)
	}
	name := created.Name

	// Enable the gateway role: 200, roles gain "gateway".
	resp = h.post(t, "/v1/nodes/"+name+"/gateway/enable", nil, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway enable status = %d, want 200", resp.StatusCode)
	}
	var enabled nodeGatewayRolesView
	decodeJSON(t, resp, &enabled)
	if diff := cmp.Diff([]string{"gateway"}, enabled.Roles); diff != "" {
		t.Errorf("enabled node roles mismatch (-want +got):\n%s", diff)
	}

	// Re-enabling is idempotent: 200, roles unchanged.
	resp = h.post(t, "/v1/nodes/"+name+"/gateway/enable", nil, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway enable (repeat) status = %d, want 200", resp.StatusCode)
	}
	var reEnabled nodeGatewayRolesView
	decodeJSON(t, resp, &reEnabled)
	if diff := cmp.Diff([]string{"gateway"}, reEnabled.Roles); diff != "" {
		t.Errorf("re-enabled node roles mismatch (-want +got):\n%s", diff)
	}

	// Disabling returns the node to hypervisor-only.
	resp = h.post(t, "/v1/nodes/"+name+"/gateway/disable", nil, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway disable status = %d, want 200", resp.StatusCode)
	}
	var disabled nodeGatewayRolesView
	decodeJSON(t, resp, &disabled)
	if diff := cmp.Diff([]string{"hypervisor"}, disabled.Roles); diff != "" {
		t.Errorf("disabled node roles mismatch (-want +got):\n%s", diff)
	}

	// Assigning the role is node:manage (admin only); an operator is refused.
	resp = h.post(t, "/v1/nodes/"+name+"/gateway/enable", nil, operator)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("operator gateway enable status = %d, want 403", resp.StatusCode)
	}
}
