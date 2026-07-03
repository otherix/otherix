// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

// nodeIngressEndpointView captures the ingress-endpoint field the full Node
// projection surfaces.
type nodeIngressEndpointView struct {
	ID                        string `json:"id"`
	Name                      string `json:"name"`
	IngressAdvertisedEndpoint string `json:"ingress_advertised_endpoint"`
}

// nodeRolesSummaryView captures the fields the roles assertions read from the
// reduced NodeSummary projection.
type nodeRolesSummaryView struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Roles []string `json:"roles"`
}

// seedRoleNode registers a ready node directly through the store, optionally
// with the gateway bit and/or a storage pool. Pool ownership is the derived
// hypervisor signal; the gateway bit is the stored gateway role. The public
// create endpoint never accepts the gateway kind and never attaches a pool, so
// the store is the seam that sets up the derivation inputs.
func seedRoleNode(t *testing.T, h *harness, prefix string, gateway, withPool bool) store.Node {
	t.Helper()
	ctx := context.Background()
	name := prefix + "-" + uuid.NewString()[:8]
	n, err := h.store.CreateNode(ctx, store.CreateNodeParams{
		ID:                      uuid.New(),
		Name:                    name,
		Gateway:                 gateway,
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://" + name + ".example.test:8443",
		MigrationHost:           "10.0.0.9",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  store.NodeStatusPending,
	})
	if err != nil {
		t.Fatalf("CreateNode(%s): %v", name, err)
	}
	if _, err := h.store.UncordonNode(ctx, n.ID); err != nil {
		t.Fatalf("UncordonNode(%s): %v", name, err)
	}
	if withPool {
		poolName := "pool-" + uuid.NewString()[:8]
		if _, err := h.store.CreateStoragePool(ctx, store.CreateStoragePoolParams{
			ID:     uuid.New(),
			NodeID: n.ID,
			Name:   poolName,
			Type:   "local_dir",
			Path:   "/var/lib/otherix/pools/" + poolName,
			Config: []byte(`{}`),
		}); err != nil {
			t.Fatalf("CreateStoragePool(%s): %v", name, err)
		}
	}
	// Re-read so the returned row reflects the uncordon (ready) status.
	fresh, err := h.store.NodeByID(ctx, n.ID)
	if err != nil {
		t.Fatalf("NodeByID(%s): %v", name, err)
	}
	return fresh
}

// TestNodeViewSurfacesRoles asserts the public node views carry the effective
// `roles` array under the pool-derived hypervisor rule: hypervisor comes from
// storage-pool ownership, gateway from the stored role. It covers GET, list,
// and the raw-row response bodies (gateway-enable and cordon), which flow
// through the separate toView path and would otherwise silently drop the
// derived hypervisor role.
func TestNodeViewSurfacesRoles(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	viewer, _ := loginAs(t, h, auth.RoleViewer)

	// A node that owns a pool derives the hypervisor role.
	hvNode := seedRoleNode(t, h, "hv", false, true)
	// A gateway node with no pool: gateway only, never a hypervisor.
	gwNode := seedRoleNode(t, h, "gw", true, false)
	// A node with neither a pool nor the gateway bit: empty role set.
	bareNode := seedRoleNode(t, h, "bare", false, false)

	// GET (full, admin): the pool-owning node is a hypervisor.
	if diff := cmp.Diff([]string{"hypervisor"}, getNodeRoles(t, h, hvNode.Name, admin)); diff != "" {
		t.Errorf("hypervisor GET roles mismatch (-want +got):\n%s", diff)
	}
	// GET (summary, viewer): the same derivation on the reduced shape.
	if diff := cmp.Diff([]string{"hypervisor"}, getNodeSummaryRoles(t, h, hvNode.Name, viewer)); diff != "" {
		t.Errorf("hypervisor summary GET roles mismatch (-want +got):\n%s", diff)
	}
	// GET: a pool-less gateway is gateway only.
	if diff := cmp.Diff([]string{"gateway"}, getNodeRoles(t, h, gwNode.Name, admin)); diff != "" {
		t.Errorf("gateway GET roles mismatch (-want +got):\n%s", diff)
	}

	// GET: a node with no pool and no gateway role carries an empty, non-null
	// `roles` array. Assert both the decoded slice and the raw JSON so a null
	// (nil slice) regression is caught.
	bareRoles, bareRaw := getNodeRolesRaw(t, h, bareNode.Name, admin)
	if diff := cmp.Diff([]string{}, bareRoles); diff != "" {
		t.Errorf("bare node GET roles mismatch (-want +got):\n%s", diff)
	}
	if !bytes.Contains(bareRaw, []byte(`"roles":[]`)) {
		t.Errorf("bare node GET roles must be the empty array, want `\"roles\":[]` in body; got %s", bareRaw)
	}

	// List: the hypervisor node surfaces its derived role in the collection view.
	if diff := cmp.Diff([]string{"hypervisor"}, listNodeRoles(t, h, admin, hvNode.ID.String())); diff != "" {
		t.Errorf("hypervisor list roles mismatch (-want +got):\n%s", diff)
	}

	// Raw-row path: enabling the gateway role on the pool-owning node must
	// return BOTH roles - the enable response renders the raw node row, so a
	// dropped hypervisor here would resurrect the derivation bug.
	resp := h.post(t, "/v1/nodes/"+hvNode.Name+"/gateway/enable", nil, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway enable status = %d, want 200", resp.StatusCode)
	}
	var enabled nodeRolesFullView
	decodeJSON(t, resp, &enabled)
	if diff := cmp.Diff([]string{"hypervisor", "gateway"}, enabled.Roles); diff != "" {
		t.Errorf("gateway-enable response roles mismatch (-want +got):\n%s", diff)
	}

	// Raw-row path: cordoning the now co-located node returns both roles too.
	resp = h.post(t, "/v1/nodes/"+hvNode.Name+"/cordon", nil, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cordon status = %d, want 200", resp.StatusCode)
	}
	var cordoned nodeRolesFullView
	decodeJSON(t, resp, &cordoned)
	if diff := cmp.Diff([]string{"hypervisor", "gateway"}, cordoned.Roles); diff != "" {
		t.Errorf("cordon response roles mismatch (-want +got):\n%s", diff)
	}
}

// getNodeRoles GETs the node by name as the bearer and returns the full view's
// roles slice.
func getNodeRoles(t *testing.T, h *harness, name, bearer string) []string {
	t.Helper()
	roles, _ := getNodeRolesRaw(t, h, name, bearer)
	return roles
}

// getNodeRolesRaw GETs the node by name and returns both the decoded roles and
// the raw response body, so a caller can distinguish an empty array from null.
func getNodeRolesRaw(t *testing.T, h *harness, name, bearer string) ([]string, []byte) {
	t.Helper()
	resp := h.get(t, "/v1/nodes/"+name, bearer)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get node %q status = %d, want 200", name, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read node %q body: %v", name, err)
	}
	var v nodeRolesFullView
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode node %q: %v", name, err)
	}
	return v.Roles, raw
}

// getNodeSummaryRoles GETs the node by name as a summary-view (developer /
// viewer) caller and returns its roles slice.
func getNodeSummaryRoles(t *testing.T, h *harness, name, bearer string) []string {
	t.Helper()
	resp := h.get(t, "/v1/nodes/"+name, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get node %q status = %d, want 200", name, resp.StatusCode)
	}
	var v nodeRolesSummaryView
	decodeJSON(t, resp, &v)
	return v.Roles
}

// listNodeRoles GETs the node collection and returns the roles of the node with
// the given id, failing if it is absent from the page.
func listNodeRoles(t *testing.T, h *harness, bearer, wantID string) []string {
	t.Helper()
	resp := h.get(t, "/v1/nodes?limit=200", bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list nodes status = %d, want 200", resp.StatusCode)
	}
	var page struct {
		Data []nodeRolesFullView `json:"data"`
	}
	decodeJSON(t, resp, &page)
	for _, n := range page.Data {
		if n.ID == wantID {
			return n.Roles
		}
	}
	t.Fatalf("node %s absent from list page", wantID)
	return nil
}

// TestNodeViewSurfacesIngressAdvertisedEndpoint asserts the full Node
// projection emits `ingress_advertised_endpoint`, the HTTPS URL clients dial
// to reach a gateway node. The public create endpoint never accepts it (a
// gateway self-registers on join), so seed the row directly through the store.
func TestNodeViewSurfacesIngressAdvertisedEndpoint(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	ctx := context.Background()

	gwName := "gw-" + uuid.NewString()[:8]
	wantIngress := "https://" + gwName + ".ingress.example.test:443"
	gw, err := h.store.CreateNode(ctx, store.CreateNodeParams{
		ID:                        uuid.New(),
		Name:                      gwName,
		Gateway:                   true,
		Architecture:              store.CPUArch("amd64"),
		AdvertisedEndpoint:        "https://" + gwName + ".example.test:8443",
		IngressAdvertisedEndpoint: wantIngress,
		MigrationHost:             "10.0.0.3",
		MigrationPortRangeStart:   49152,
		MigrationPortRangeEnd:     49251,
		Status:                    store.NodeStatusPending,
	})
	if err != nil {
		t.Fatalf("CreateNode(gateway): %v", err)
	}

	resp := h.get(t, "/v1/nodes/"+gw.Name, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin get gateway node status = %d, want 200", resp.StatusCode)
	}
	var view nodeIngressEndpointView
	decodeJSON(t, resp, &view)
	if view.IngressAdvertisedEndpoint != wantIngress {
		t.Errorf("ingress_advertised_endpoint = %q, want %q", view.IngressAdvertisedEndpoint, wantIngress)
	}
}
