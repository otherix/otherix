// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"crypto"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// ingressResp mirrors the broker response the CLI consumes: a transport
// discriminator plus the connect coordinates.
type ingressResp struct {
	Transport   string `json:"transport"`
	VMID        string `json:"vm_id"`
	Port        int    `json:"port"`
	SplicerAddr string `json:"splicer_addr"`
	SessionCred string `json:"session_cred"`
	ExpiresAt   string `json:"expires_at"`
}

// seedIngressOverlayVM creates an overlay VM with a NIC (and a CP-IPAM IPv4)
// owned by the admin token, returning its name, id, overlay network id, and the
// compute node it is bound to.
func seedIngressOverlayVM(t *testing.T, h *harness, admin string, adminID uuid.UUID) (vmName string, vmID, netID, nodeID uuid.UUID) {
	t.Helper()
	nodeID, poolName := schedulableFixtureWithNode(t, h, adminID)
	ov := overlayDhcpNetwork(t, h, admin, "10.80.0.0/24")
	netID = uuid.MustParse(ov.ID)
	vmName = bindVMOnNetwork(t, h, admin, nodeID, netID, poolName, ov.Name)
	vmID = vmIDByName(t, h.store, vmName)
	return vmName, vmID, netID, nodeID
}

// convergeGateway stands up a live gateway node that is a member of netID and
// has reported that overlay's reconciliation converged ("ready"), so
// SelectGatewayForVM can choose it. Returns the gateway node.
func convergeGateway(t *testing.T, h *harness, netID uuid.UUID) store.Node {
	t.Helper()
	ctx := context.Background()
	gw, err := h.store.CreateNode(ctx, store.CreateNodeParams{
		ID: uuid.New(), Name: "gw-" + uuid.NewString()[:8], Kind: store.NodeKindGateway,
		Architecture: store.CpuArchAmd64, AdvertisedEndpoint: "https://gw.test:9443",
		MigrationHost: "10.0.0.9", MigrationPortRangeStart: 49152, MigrationPortRangeEnd: 49251,
		Status: store.NodeStatusReady,
	})
	if err != nil {
		t.Fatalf("CreateNode(gateway): %v", err)
	}
	if _, err := h.store.CreateGatewayMembership(ctx, gw.ID, netID); err != nil {
		t.Fatalf("CreateGatewayMembership: %v", err)
	}
	if err := h.store.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
		NetworkID: netID, NodeID: gw.ID, ReconciliationStatus: "ready",
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus(gateway ready): %v", err)
	}
	return gw
}

// seedSessionCA provisions the cluster ingress-session CA and returns its public
// half so a test can verify a minted credential.
func seedSessionCA(t *testing.T, h *harness) crypto.PublicKey {
	t.Helper()
	mat, err := auth.GenerateSessionCA()
	if err != nil {
		t.Fatalf("GenerateSessionCA: %v", err)
	}
	if _, err := h.store.CreateSessionCA(context.Background(), store.CreateSessionCAParams{
		PrivateKeyPEM: mat.PrivateKeyPEM, PublicKeyPEM: mat.PublicKeyPEM,
	}); err != nil {
		t.Fatalf("CreateSessionCA: %v", err)
	}
	pub, err := auth.ParseSessionCAPublic(mat.PublicKeyPEM)
	if err != nil {
		t.Fatalf("ParseSessionCAPublic: %v", err)
	}
	return pub
}

// TestVMIngress_OverlayGateway drives the broker happy path: an operator brokers
// their own overlay VM:22, the CP selects the converged gateway, mints a session
// credential, and returns the gateway splicer address. The credential must verify
// against the session CA public half and carry the VM's id, NIC MAC, guest IP,
// and port.
func TestVMIngress_OverlayGateway(t *testing.T) {
	h := newE2E(t)
	ctx := context.Background()
	admin, adminID := loginAs(t, h, auth.RoleAdmin)

	vmName, vmID, netID, _ := seedIngressOverlayVM(t, h, admin, adminID)
	gw := convergeGateway(t, h, netID)
	caPub := seedSessionCA(t, h)

	nics, err := h.store.ListVMNicsByVM(ctx, vmID)
	if err != nil || len(nics) == 0 {
		t.Fatalf("ListVMNicsByVM: nics=%d err=%v", len(nics), err)
	}
	nic := nics[0]
	if nic.Ipv4Address == nil {
		t.Fatalf("overlay NIC has no IPv4; CP-IPAM did not allocate")
	}

	resp := h.post(t, "/v1/vms/"+vmName+"/ingress", map[string]any{"port": 22}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ingress status = %d, want 200", resp.StatusCode)
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
	if out.Port != 22 {
		t.Errorf("port = %d, want 22", out.Port)
	}
	if out.SessionCred == "" {
		t.Fatal("session_cred is empty")
	}

	claims, err := auth.VerifySessionCred(caPub, out.SessionCred, time.Now())
	if err != nil {
		t.Fatalf("VerifySessionCred: %v", err)
	}
	if claims.VMID != vmID {
		t.Errorf("cred VMID = %v, want %v", claims.VMID, vmID)
	}
	if claims.NICMAC != nic.MacAddress.String() {
		t.Errorf("cred NICMAC = %q, want %q", claims.NICMAC, nic.MacAddress.String())
	}
	if claims.GuestIP != *nic.Ipv4Address {
		t.Errorf("cred GuestIP = %v, want %v", claims.GuestIP, *nic.Ipv4Address)
	}
	if claims.Port != 22 {
		t.Errorf("cred Port = %d, want 22", claims.Port)
	}
}

// TestVMIngress_ForeignVMDeveloper404 proves a developer brokering a VM they do
// not own gets 404 (no existence leak), never 403.
func TestVMIngress_ForeignVMDeveloper404(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	devToken, _ := loginAs(t, h, auth.RoleDeveloper)

	vmName, _, netID, _ := seedIngressOverlayVM(t, h, admin, adminID)
	convergeGateway(t, h, netID)
	seedSessionCA(t, h)

	resp := h.post(t, "/v1/vms/"+vmName+"/ingress", map[string]any{"port": 22}, devToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign-vm ingress status = %d, want 404", resp.StatusCode)
	}
	var body errorEnvelope
	decodeJSON(t, resp, &body)
	if body.Error.Code != "not_found" && body.Error.Code != "vm_not_found" {
		t.Errorf("error code = %q, want not_found", body.Error.Code)
	}
}

// TestVMIngress_NoGateway409 proves an overlay VM with no converged gateway is
// 409 ingress_unavailable.
func TestVMIngress_NoGateway409(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)

	vmName, _, _, _ := seedIngressOverlayVM(t, h, admin, adminID)
	seedSessionCA(t, h)

	resp := h.post(t, "/v1/vms/"+vmName+"/ingress", map[string]any{"port": 22}, admin)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("no-gateway ingress status = %d, want 409", resp.StatusCode)
	}
	var body errorEnvelope
	decodeJSON(t, resp, &body)
	if body.Error.Code != "ingress_unavailable" {
		t.Errorf("error code = %q, want ingress_unavailable", body.Error.Code)
	}
}

// TestVMIngress_BridgeRelay proves a bridge VM is brokered over the CP-relay
// transport: transport=relay, no gateway credential.
func TestVMIngress_BridgeRelay(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)

	nodeID, poolName := schedulableFixtureWithNode(t, h, adminID)
	br := bridgeNetwork(t, h, admin)
	vmName := bindVMOnNetwork(t, h, admin, nodeID, uuid.MustParse(br.ID), poolName, br.Name)
	seedSessionCA(t, h)

	resp := h.post(t, "/v1/vms/"+vmName+"/ingress", map[string]any{"port": 8080}, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bridge ingress status = %d, want 200", resp.StatusCode)
	}
	var out ingressResp
	decodeJSON(t, resp, &out)
	if out.Transport != "relay" {
		t.Errorf("transport = %q, want relay", out.Transport)
	}
	if out.Port != 8080 {
		t.Errorf("port = %d, want 8080", out.Port)
	}
	if out.SessionCred != "" {
		t.Errorf("session_cred = %q, want empty on the relay path", out.SessionCred)
	}
	if out.SplicerAddr != "" {
		t.Errorf("splicer_addr = %q, want empty on the relay path", out.SplicerAddr)
	}
}

// TestVMIngress_InvalidPort400 proves an out-of-range port is 400
// validation_failed.
func TestVMIngress_InvalidPort400(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	vmName, _, _, _ := seedIngressOverlayVM(t, h, admin, adminID)

	for _, port := range []int{0, 70000} {
		resp := h.post(t, "/v1/vms/"+vmName+"/ingress", map[string]any{"port": port}, admin)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("port=%d ingress status = %d, want 400", port, resp.StatusCode)
		}
		var body errorEnvelope
		decodeJSON(t, resp, &body)
		if body.Error.Code != "validation_failed" {
			t.Errorf("port=%d error code = %q, want validation_failed", port, body.Error.Code)
		}
	}
}
