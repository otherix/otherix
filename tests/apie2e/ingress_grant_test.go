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
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// seedIngressGrantSourceIP creates a grant scoped to {vmName: [22]} pinned to
// sourceIP (a CIDR or a bare address) and returns the plaintext token. The pin
// is evaluated against the caller's RemoteAddr at broker time.
func seedIngressGrantSourceIP(t *testing.T, s *etcdstore.Store, creator uuid.UUID, vmName, sourceIP string) string {
	t.Helper()
	plaintext, hash, err := auth.GenerateIngressGrantToken()
	if err != nil {
		t.Fatalf("GenerateIngressGrantToken: %v", err)
	}
	pin := sourceIP
	if _, err := s.CreateIngressGrant(context.Background(), store.CreateIngressGrantParams{
		Name:      "grant-" + uuid.NewString()[:8],
		CreatedBy: creator,
		TokenHash: hash,
		VMs:       []store.IngressGrantVM{{VMName: vmName, Ports: []int{22}, Login: "ci"}},
		SourceIP:  &pin,
	}); err != nil {
		t.Fatalf("CreateIngressGrant: %v", err)
	}
	return plaintext
}

// TestIngressGrant_OverlayGateway drives the grant-authorized broker happy path
// on an overlay VM: an ingress-grant token scoped to {vm: [22]} brokers vm:22,
// the CP selects the converged gateway and mints a session credential. The
// caller is not an Authn principal - authorization is entirely the grant.
func TestIngressGrant_OverlayGateway(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)

	vmName, vmID, netID, _ := seedIngressOverlayVM(t, h, admin, adminID)
	gw := convergeGateway(t, h, netID)
	seedSessionCA(t, h)

	grantTok := seedIngressGrant(t, h.store, adminID, vmName, "ci")

	resp := h.post(t, "/v1/vms/"+vmName+"/ingress", map[string]any{"port": 22}, grantTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("grant ingress status = %d, want 200", resp.StatusCode)
	}
	var out ingressResp
	decodeJSON(t, resp, &out)
	if out.Transport != "gateway" {
		t.Errorf("transport = %q, want gateway", out.Transport)
	}
	if out.SplicerAddr != gw.AdvertisedEndpoint {
		t.Errorf("splicer_addr = %q, want %q", out.SplicerAddr, gw.AdvertisedEndpoint)
	}
	if out.VMID != vmID.String() {
		t.Errorf("vm_id = %q, want %q", out.VMID, vmID)
	}
	if out.SessionCred == "" {
		t.Fatal("session_cred is empty on the gateway path")
	}
}

// TestIngressGrant_PortOutOfScope404 proves a grant scoped to {vm: [22]} does
// not authorize a different guest port: brokering vm:8080 collapses to the
// uniform 404, leaking neither VM existence nor grant scope.
func TestIngressGrant_PortOutOfScope404(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)

	vmName, _, netID, _ := seedIngressOverlayVM(t, h, admin, adminID)
	convergeGateway(t, h, netID)
	seedSessionCA(t, h)

	grantTok := seedIngressGrant(t, h.store, adminID, vmName, "ci")

	resp := h.post(t, "/v1/vms/"+vmName+"/ingress", map[string]any{"port": 8080}, grantTok)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("out-of-scope-port ingress status = %d, want 404", resp.StatusCode)
	}
	var body errorEnvelope
	decodeJSON(t, resp, &body)
	if body.Error.Code != "vm_not_found" && body.Error.Code != "not_found" {
		t.Errorf("error code = %q, want vm_not_found", body.Error.Code)
	}
}

// TestIngressGrant_SourceIPPin proves the source-IP pin is enforced on the
// grant path: a pin covering the loopback test client authorizes (200), a pin
// excluding it collapses to the uniform 404. Both share the same grant scope
// and VM, so the only variable is the pin - the 404 is the pin firing, not a
// scope miss.
func TestIngressGrant_SourceIPPin(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)

	vmName, _, netID, _ := seedIngressOverlayVM(t, h, admin, adminID)
	convergeGateway(t, h, netID)
	seedSessionCA(t, h)

	// A pin covering the loopback client (httptest dials from 127.0.0.1)
	// authorizes; a pin excluding it (a private-range CIDR loopback is not in)
	// denies with the uniform 404.
	allowTok := seedIngressGrantSourceIP(t, h.store, adminID, vmName, "127.0.0.0/8")
	denyTok := seedIngressGrantSourceIP(t, h.store, adminID, vmName, "10.0.0.0/8")

	allowResp := h.post(t, "/v1/vms/"+vmName+"/ingress", map[string]any{"port": 22}, allowTok)
	if allowResp.StatusCode != http.StatusOK {
		t.Fatalf("in-pin ingress status = %d, want 200", allowResp.StatusCode)
	}
	allowResp.Body.Close()

	denyResp := h.post(t, "/v1/vms/"+vmName+"/ingress", map[string]any{"port": 22}, denyTok)
	if denyResp.StatusCode != http.StatusNotFound {
		t.Fatalf("out-of-pin ingress status = %d, want 404", denyResp.StatusCode)
	}
	var body errorEnvelope
	decodeJSON(t, denyResp, &body)
	if body.Error.Code != "vm_not_found" && body.Error.Code != "not_found" {
		t.Errorf("error code = %q, want vm_not_found", body.Error.Code)
	}
}

// TestIngressGrant_BridgeRelay proves a grant brokers a bridge VM over the
// CP-relay transport: transport=relay, no gateway credential minted. The relay
// itself authorizes each ssh-stream connect per request (that port-scope
// enforcement is exercised by the ssh_stream package tests).
func TestIngressGrant_BridgeRelay(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)

	nodeID, poolName := schedulableFixtureWithNode(t, h, adminID)
	br := bridgeNetwork(t, h, admin)
	vmName := bindVMOnNetwork(t, h, admin, nodeID, uuid.MustParse(br.ID), poolName, br.Name)

	grantTok := seedIngressGrant(t, h.store, adminID, vmName, "ci")

	resp := h.post(t, "/v1/vms/"+vmName+"/ingress", map[string]any{"port": 22}, grantTok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bridge grant ingress status = %d, want 200", resp.StatusCode)
	}
	var out ingressResp
	decodeJSON(t, resp, &out)
	if out.Transport != "relay" {
		t.Errorf("transport = %q, want relay", out.Transport)
	}
	if out.SessionCred != "" {
		t.Errorf("session_cred = %q, want empty on the relay path", out.SessionCred)
	}
	if out.SplicerAddr != "" {
		t.Errorf("splicer_addr = %q, want empty on the relay path", out.SplicerAddr)
	}
}

// TestIngressGrant_IdentityViewer403 proves the vm:connect capability check
// survived moving the route out from under RequirePermission: a viewer (a role
// without vm:connect) brokering any VM gets 403 permission_denied from the
// handler, not the uniform 404. This is the discipline the middleware used to
// enforce.
func TestIngressGrant_IdentityViewer403(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	viewer, _ := loginAs(t, h, auth.RoleViewer)

	vmName, _, netID, _ := seedIngressOverlayVM(t, h, admin, adminID)
	convergeGateway(t, h, netID)
	seedSessionCA(t, h)

	resp := h.post(t, "/v1/vms/"+vmName+"/ingress", map[string]any{"port": 22}, viewer)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer ingress status = %d, want 403", resp.StatusCode)
	}
	var body errorEnvelope
	decodeJSON(t, resp, &body)
	if body.Error.Code != "permission_denied" {
		t.Errorf("error code = %q, want permission_denied", body.Error.Code)
	}
}
