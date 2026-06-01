// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// nodeNetworkConditionView mirrors the NodeNetworkCondition wire schema
// surfaced on GET /v1/nodes/{id} for admin / operator callers.
type nodeNetworkConditionView struct {
	NetworkID            string  `json:"network_id"`
	Name                 string  `json:"name"`
	ReconciliationStatus string  `json:"reconciliation_status"`
	ReconciliationError  *string `json:"reconciliation_error"`
	LastReconciledAt     *string `json:"last_reconciled_at"`
}

// nodeFullView captures only the fields these tests assert on. The full
// Node projection has many more fields; decoding a subset is fine.
type nodeFullView struct {
	ID                string                     `json:"id"`
	Name              string                     `json:"name"`
	NetworkConditions []nodeNetworkConditionView `json:"network_conditions"`
}

func newNodeBody() map[string]any {
	suffix := uuid.NewString()[:8]
	return map[string]any{
		"name":                       "node-" + suffix,
		"architecture":               "amd64",
		"advertised_endpoint":        "https://node-" + suffix + ".example.test:8443",
		"migration_host":             "10.0.0.1",
		"migration_port_range_start": 49152,
		"migration_port_range_end":   49251,
	}
}

func TestNodeGetSurfacesNetworkConditionsAsAdmin(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	ctx := context.Background()

	// Seed a node via the API and grab its id from the full view.
	resp := h.post(t, "/v1/nodes", newNodeBody(), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create node status = %d, want 201", resp.StatusCode)
	}
	var node nodeFullView
	decodeJSON(t, resp, &node)
	if node.ID == "" {
		t.Fatalf("node id empty: %+v", node)
	}
	nodeID, err := uuid.Parse(node.ID)
	if err != nil {
		t.Fatalf("parse node id: %v", err)
	}

	// Seed a network.
	netID := uuid.New()
	if _, err := h.store.CreateNetwork(ctx, store.CreateNetworkParams{
		ID:         netID,
		Name:       "vnet-cond",
		Type:       store.NetworkTypeBridge,
		BridgeName: "brcond",
		Mtu:        1500,
	}); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}

	// Inject a failed per-(node, network) materialisation status.
	wantErr := "bridge brcond not present"
	if err := h.store.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
		NetworkID:            netID,
		NodeID:               nodeID,
		ReconciliationStatus: "failed",
		ReconciliationError:  &wantErr,
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus: %v", err)
	}

	resp = h.get(t, "/v1/nodes/"+node.Name, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get node status = %d, want 200", resp.StatusCode)
	}
	var got nodeFullView
	decodeJSON(t, resp, &got)
	if len(got.NetworkConditions) != 1 {
		t.Fatalf("network_conditions len = %d, want 1 (%+v)", len(got.NetworkConditions), got.NetworkConditions)
	}
	c := got.NetworkConditions[0]
	if c.NetworkID != netID.String() {
		t.Errorf("condition network_id = %q, want %q", c.NetworkID, netID.String())
	}
	if c.Name != "vnet-cond" {
		t.Errorf("condition name = %q, want %q", c.Name, "vnet-cond")
	}
	if c.ReconciliationStatus != "failed" {
		t.Errorf("condition status = %q, want failed", c.ReconciliationStatus)
	}
	if c.ReconciliationError == nil || *c.ReconciliationError != wantErr {
		t.Errorf("condition error = %v, want %q", c.ReconciliationError, wantErr)
	}
}

func TestNodeGetNetworkConditionsHiddenForViewer(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	viewer, _ := loginAs(t, h, auth.RoleViewer)
	ctx := context.Background()

	resp := h.post(t, "/v1/nodes", newNodeBody(), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create node status = %d, want 201", resp.StatusCode)
	}
	var node nodeFullView
	decodeJSON(t, resp, &node)
	nodeID := uuid.MustParse(node.ID)

	netID := uuid.New()
	if _, err := h.store.CreateNetwork(ctx, store.CreateNetworkParams{
		ID:         netID,
		Name:       "vnet-hidden",
		Type:       store.NetworkTypeBridge,
		BridgeName: "brhid",
		Mtu:        1500,
	}); err != nil {
		t.Fatalf("CreateNetwork: %v", err)
	}
	if err := h.store.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
		NetworkID:            netID,
		NodeID:               nodeID,
		ReconciliationStatus: "ready",
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus: %v", err)
	}

	// Viewer gets the summary view; the field must be absent. Decode the
	// raw body into a map so an absent key is distinguishable from null.
	resp = h.get(t, "/v1/nodes/"+node.Name, viewer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get node status = %d, want 200", resp.StatusCode)
	}
	var raw map[string]any
	decodeJSON(t, resp, &raw)
	if _, ok := raw["network_conditions"]; ok {
		t.Errorf("summary view leaked network_conditions: %v", raw["network_conditions"])
	}
}

func TestNodeGetSkipsDeletedNetworkConditions(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	ctx := context.Background()

	resp := h.post(t, "/v1/nodes", newNodeBody(), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create node status = %d, want 201", resp.StatusCode)
	}
	var node nodeFullView
	decodeJSON(t, resp, &node)
	nodeID := uuid.MustParse(node.ID)

	// A status row pointing at a network that does not exist (deleted or
	// never created) must be silently skipped, not 500.
	ghostNet := uuid.New()
	if err := h.store.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
		NetworkID:            ghostNet,
		NodeID:               nodeID,
		ReconciliationStatus: "failed",
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus: %v", err)
	}

	resp = h.get(t, "/v1/nodes/"+node.Name, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get node status = %d, want 200", resp.StatusCode)
	}
	var got nodeFullView
	decodeJSON(t, resp, &got)
	if len(got.NetworkConditions) != 0 {
		t.Errorf("network_conditions = %+v, want empty (stale entry skipped)", got.NetworkConditions)
	}
}
