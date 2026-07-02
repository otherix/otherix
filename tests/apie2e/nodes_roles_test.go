// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// nodeRolesFullView captures the fields the roles assertions read from the
// full Node projection.
type nodeRolesFullView struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
}

// nodeRolesSummaryView captures the fields the roles assertions read from the
// reduced NodeSummary projection.
type nodeRolesSummaryView struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
}

// TestNodeViewSurfacesRoles asserts the public node views carry the derived
// `roles` array: a gateway node reports ["gateway"], a default node reports
// ["hypervisor"], on both the full (admin) and summary (viewer) shapes.
func TestNodeViewSurfacesRoles(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	viewer, _ := loginAs(t, h, auth.RoleViewer)
	ctx := context.Background()

	// A default node created through the public API is a hypervisor.
	resp := h.post(t, "/v1/nodes", newNodeBody(), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create hypervisor node status = %d, want 201", resp.StatusCode)
	}
	var hv nodeRolesFullView
	decodeJSON(t, resp, &hv)
	if diff := cmp.Diff([]string{"hypervisor"}, hv.Roles); diff != "" {
		t.Errorf("hypervisor node roles mismatch (-want +got):\n%s", diff)
	}

	// A gateway node self-registers on join; seed one directly through the
	// store since the public create endpoint never accepts the gateway kind.
	gwName := "gw-" + uuid.NewString()[:8]
	gw, err := h.store.CreateNode(ctx, store.CreateNodeParams{
		ID:                      uuid.New(),
		Name:                    gwName,
		Gateway:                 true,
		Architecture:            store.CPUArch("amd64"),
		AdvertisedEndpoint:      "https://" + gwName + ".example.test:8443",
		MigrationHost:           "10.0.0.2",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  store.NodeStatusPending,
	})
	if err != nil {
		t.Fatalf("CreateNode(gateway): %v", err)
	}

	// Full view (admin) carries roles == ["gateway"].
	resp = h.get(t, "/v1/nodes/"+gw.Name, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin get gateway node status = %d, want 200", resp.StatusCode)
	}
	var gwView nodeRolesFullView
	decodeJSON(t, resp, &gwView)
	if diff := cmp.Diff([]string{"gateway"}, gwView.Roles); diff != "" {
		t.Errorf("gateway node roles mismatch (-want +got):\n%s", diff)
	}

	// Summary view (viewer) still carries roles on both nodes.
	resp = h.get(t, "/v1/nodes/"+gw.Name, viewer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer get gateway node status = %d, want 200", resp.StatusCode)
	}
	var gwSummary nodeRolesSummaryView
	decodeJSON(t, resp, &gwSummary)
	if diff := cmp.Diff([]string{"gateway"}, gwSummary.Roles); diff != "" {
		t.Errorf("gateway summary roles mismatch (-want +got):\n%s", diff)
	}

	resp = h.get(t, "/v1/nodes/"+hv.Name, viewer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer get hypervisor node status = %d, want 200", resp.StatusCode)
	}
	var hvSummary nodeRolesSummaryView
	decodeJSON(t, resp, &hvSummary)
	if diff := cmp.Diff([]string{"hypervisor"}, hvSummary.Roles); diff != "" {
		t.Errorf("hypervisor summary roles mismatch (-want +got):\n%s", diff)
	}
}
