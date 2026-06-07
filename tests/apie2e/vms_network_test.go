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

// imageURL is the canonical image source the apie2e VM-create bodies reference.
// Placement does not fetch it; only the self-describing VM row records it.
const imageURL = "https://example.test/img.qcow2"

// vmCreateBody builds the image-model VM-create request body the create handler
// expects: name + image_url + arch (no template field). Callers merge in pool /
// network / vcpus / memory_mb as needed.
func vmCreateBody(extra map[string]any) map[string]any {
	body := map[string]any{
		"name": "vm-" + uuid.NewString()[:8], "image_url": imageURL, "arch": "amd64",
		"vcpus": 2, "memory_mb": 2048,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// schedulableFixture seeds a ready node, a local_dir pool on it, and a default
// uefi firmware (so the image-model create resolves firmware), returning the
// pool name the create body references. Mirrors the etcdstore schedulingFixture;
// placement passes on the count-based fallback path (no heartbeat metrics
// required).
func schedulableFixture(t *testing.T, h *harness, owner uuid.UUID) (poolName string) {
	_, poolName = schedulableFixtureWithNode(t, h, owner)
	return poolName
}

// schedulableFixtureWithNode is schedulableFixture exposing the node id so
// callers can seed per-(node, network) reconciliation status for the
// network-aware placement filter (ADR 0034 NL18).
func schedulableFixtureWithNode(t *testing.T, h *harness, owner uuid.UUID) (nodeID uuid.UUID, poolName string) {
	t.Helper()
	ctx := context.Background()
	s := h.store

	nodeID = uuid.New()
	if _, err := s.CreateNode(ctx, store.CreateNodeParams{
		ID: nodeID, Name: "node-" + uuid.NewString()[:8], Architecture: store.CpuArchAmd64,
		AdvertisedEndpoint: "https://node.test:9443", MigrationHost: "10.0.0.1",
		MigrationPortRangeStart: 49152, MigrationPortRangeEnd: 49251, Status: store.NodeStatusPending,
	}); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if _, err := s.UncordonNode(ctx, nodeID); err != nil {
		t.Fatalf("UncordonNode: %v", err)
	}

	poolName = "pool-" + uuid.NewString()[:8]
	if _, err := s.CreateStoragePool(ctx, store.CreateStoragePoolParams{
		ID: uuid.New(), NodeID: nodeID, Name: poolName, Type: "local_dir",
		Path: "/opt/otherix/pools/" + poolName, Config: []byte(`{}`),
	}); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}

	seedDefaultFirmware(t, h)
	return nodeID, poolName
}

// seedDefaultFirmware seeds a default uefi firmware for amd64 so the image-model
// VM create resolves firmware via DefaultFirmwareForArchType. Idempotent across
// the unique-default guard: a pre-existing default makes the second call a
// no-op (the e2e harness is single-node per test, so the first call wins).
func seedDefaultFirmware(t *testing.T, h *harness) {
	t.Helper()
	if _, err := h.store.DefaultFirmwareForArchType(context.Background(), store.CpuArchAmd64, store.FirmwareTypeUefi); err == nil {
		return
	}
	if _, err := h.store.CreateFirmware(context.Background(), store.CreateFirmwareParams{
		ID: uuid.New(), Name: "fw-" + uuid.NewString()[:8], Architecture: store.CpuArchAmd64,
		Type: store.FirmwareTypeUefi, IsDefault: true,
	}); err != nil {
		t.Fatalf("CreateFirmware: %v", err)
	}
}

func bridgeNetwork(t *testing.T, h *harness, admin string) networkView {
	t.Helper()
	resp := h.post(t, "/v1/networks", newNetworkBody(), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create network status = %d, want 201", resp.StatusCode)
	}
	var net networkView
	decodeJSON(t, resp, &net)
	return net
}

// TestVMCreateStashesNetworkNamePending asserts admission stashes the requested
// network NAME on the VM's SchedulingSpec and returns 201 pending: the NIC row
// is not created at admission (the vms.schedule loop mints it at bind), so the
// public view surfaces the requested network name from the spec while the VM is
// pending.
func TestVMCreateStashesNetworkNamePending(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	poolName := schedulableFixture(t, h, adminID)
	net := bridgeNetwork(t, h, admin)

	body := vmCreateBody(map[string]any{"pool": poolName, "network": net.Name})
	vmName := body["name"].(string)
	resp := h.post(t, "/v1/vms", body, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create vm status = %d, want 201", resp.StatusCode)
	}
	var created struct {
		Networks []string `json:"networks"`
		Status   struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	decodeJSON(t, resp, &created)
	if created.Status.Phase != "pending" {
		t.Errorf("status.phase = %q, want pending", created.Status.Phase)
	}

	// No NIC row exists yet (bind is deferred to the schedule loop).
	vmRow, err := h.store.VMByName(context.Background(), vmName)
	if err != nil {
		t.Fatalf("VMByName: %v", err)
	}
	nics, err := h.store.ListVMNicsByVM(context.Background(), vmRow.ID)
	if err != nil {
		t.Fatalf("ListVMNicsByVM: %v", err)
	}
	if len(nics) != 0 {
		t.Errorf("nics = %d, want 0 (NIC minted at bind, not admission)", len(nics))
	}

	// The pending view surfaces the requested network name from the spec.
	if diff := cmp.Diff([]string{net.Name}, created.Networks); diff != "" {
		t.Errorf("pending networks mismatch (-want +got):\n%s", diff)
	}
}

// TestVMViewSurfacesNetworks drives the real GET /v1/vms/{name} and
// GET /v1/vms projection over etcdstore and asserts the `networks`
// field reflects the attached NIC's network (the seam the CLI NETWORK
// column reads). A VM created without --network surfaces an empty
// array, not null.
func TestVMViewSurfacesNetworks(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)

	// VM with one NIC on a bridge network.
	nodeID, poolName := schedulableFixtureWithNode(t, h, adminID)
	net := bridgeNetwork(t, h, admin)
	if err := h.store.UpsertNetworkNodeStatus(context.Background(), store.UpsertNetworkNodeStatusParams{
		NetworkID: uuid.MustParse(net.ID), NodeID: nodeID, ReconciliationStatus: "ready",
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus: %v", err)
	}
	withNet := vmCreateBody(map[string]any{"pool": poolName, "network": net.Name})
	resp := h.post(t, "/v1/vms", withNet, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create vm status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()
	withNetName := withNet["name"].(string)

	var got vmNetworksView
	getResp := h.get(t, "/v1/vms/"+withNetName, admin)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get vm status = %d, want 200", getResp.StatusCode)
	}
	decodeJSON(t, getResp, &got)
	if diff := cmp.Diff([]string{net.Name}, got.Networks); diff != "" {
		t.Errorf("vm get networks mismatch (-want +got):\n%s", diff)
	}

	// VM without a NIC: networks must be the empty array, never null.
	noNetPool := schedulableFixture(t, h, adminID)
	noNet := vmCreateBody(map[string]any{"pool": noNetPool})
	resp = h.post(t, "/v1/vms", noNet, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create vm status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	var bare vmNetworksView
	getResp = h.get(t, "/v1/vms/"+noNet["name"].(string), admin)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("get vm status = %d, want 200", getResp.StatusCode)
	}
	decodeJSON(t, getResp, &bare)
	if bare.Networks == nil {
		t.Error("vm get networks = null, want []")
	}
	if len(bare.Networks) != 0 {
		t.Errorf("vm get networks = %v, want []", bare.Networks)
	}
}

// vmNetworksView captures only the networks field of the VM projection.
type vmNetworksView struct {
	Networks []string `json:"networks"`
}

// vmOwnerView captures the owner-identity fields of the VM projection.
type vmOwnerView struct {
	OwnerID string  `json:"owner_id"`
	Owner   *string `json:"owner"`
}

// TestVMViewOwnerGatedByUserRead asserts the owner display_name is
// resolved onto the VM view only for callers holding user:read: an admin
// sees `owner`, while a developer (who lacks user:read) sees `owner` =
// null with the raw `owner_id` still present. owner_id is always set so
// the VM surface never becomes a back door into the user directory for
// roles that cannot read it.
func TestVMViewOwnerGatedByUserRead(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	developer, _ := loginAs(t, h, auth.RoleDeveloper)
	poolName := schedulableFixture(t, h, adminID)

	body := vmCreateBody(map[string]any{"pool": poolName})
	resp := h.post(t, "/v1/vms", body, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create vm status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()
	vmName := body["name"].(string)

	// Admin holds user:read -> owner resolves to the owner's display_name.
	var asAdmin vmOwnerView
	getResp := h.get(t, "/v1/vms/"+vmName, admin)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("admin get vm status = %d, want 200", getResp.StatusCode)
	}
	decodeJSON(t, getResp, &asAdmin)
	if asAdmin.OwnerID != adminID.String() {
		t.Errorf("admin view owner_id = %q, want %q", asAdmin.OwnerID, adminID)
	}
	if asAdmin.Owner == nil || *asAdmin.Owner != "E2E "+string(auth.RoleAdmin) {
		t.Errorf("admin view owner = %v, want %q", asAdmin.Owner, "E2E "+string(auth.RoleAdmin))
	}

	// Developer lacks user:read -> owner is null, owner_id still present.
	var asDev vmOwnerView
	getResp = h.get(t, "/v1/vms/"+vmName, developer)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("developer get vm status = %d, want 200", getResp.StatusCode)
	}
	decodeJSON(t, getResp, &asDev)
	if asDev.Owner != nil {
		t.Errorf("developer view owner = %v, want null (lacks user:read)", *asDev.Owner)
	}
	if asDev.OwnerID != adminID.String() {
		t.Errorf("developer view owner_id = %q, want %q", asDev.OwnerID, adminID)
	}
}

func TestVMCreateWithoutNetworkHasNoNic(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	poolName := schedulableFixture(t, h, adminID)

	body := vmCreateBody(map[string]any{"pool": poolName})
	vmName := body["name"].(string)
	resp := h.post(t, "/v1/vms", body, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create vm status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	vmRow, err := h.store.VMByName(context.Background(), vmName)
	if err != nil {
		t.Fatalf("VMByName: %v", err)
	}
	nics, err := h.store.ListVMNicsByVM(context.Background(), vmRow.ID)
	if err != nil {
		t.Fatalf("ListVMNicsByVM: %v", err)
	}
	if len(nics) != 0 {
		t.Errorf("nics = %d, want 0 (no --network)", len(nics))
	}
}

// TestVMCreateUnknownNetworkNameDeferred asserts the admission-only contract for
// the network field: an explicit network UUID that misses still fails fast with
// 404 (an explicit reference), but a bare network NAME that does not exist is
// deferred (stashed on the SchedulingSpec, resolved at bind) and admission
// returns 201 pending - the unknown name becomes a scheduling reason later, not
// an admission error.
func TestVMCreateUnknownNetworkNameDeferred(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	poolName := schedulableFixture(t, h, adminID)

	// Bare name miss -> deferred -> 201 pending.
	resp := h.post(t, "/v1/vms", vmCreateBody(map[string]any{
		"pool": poolName, "network": "does-not-exist",
	}), admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create vm (unknown network name) = %d, want 201 (deferred)", resp.StatusCode)
	}
	resp.Body.Close()

	// Explicit UUID miss -> still fails fast with 404.
	resp = h.post(t, "/v1/vms", vmCreateBody(map[string]any{
		"pool": poolName, "network": uuid.NewString(),
	}), admin)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("create vm (unknown network uuid) = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}
