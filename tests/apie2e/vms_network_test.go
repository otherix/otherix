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

// vmCreateAcceptedView is the 202 AsyncTaskAccepted envelope the create handler
// returns. The async pipeline is not driven in apie2e (no worker dispatcher);
// the assertions key off the synchronously-committed vms + vm_nics rows.
type vmCreateAcceptedView struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

// schedulableFixture seeds a ready node, a local_dir pool on it, and a template
// owned by the given user, returning the names the create body references.
// Mirrors the etcdstore schedulingFixture; placement passes on the count-based
// fallback path (no heartbeat metrics required).
func schedulableFixture(t *testing.T, h *harness, owner uuid.UUID) (poolName, templateName string) {
	_, poolName, templateName = schedulableFixtureWithNode(t, h, owner)
	return poolName, templateName
}

// schedulableFixtureWithNode is schedulableFixture exposing the node id so
// callers can seed per-(node, network) reconciliation status for the
// network-aware placement filter (ADR 0034 NL18).
func schedulableFixtureWithNode(t *testing.T, h *harness, owner uuid.UUID) (nodeID uuid.UUID, poolName, templateName string) {
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

	templateName = "tpl-" + uuid.NewString()[:8]
	if _, err := s.CreateTemplate(ctx, store.CreateTemplateParams{
		ID: uuid.New(), OwnerID: owner, Name: templateName, Description: "e2e",
		Architecture: store.CpuArchAmd64, OsFamily: store.OsFamilyLinux, OsVariant: "noble",
		ImageUrl: "https://example.test/img.qcow2", ImageFormat: store.ImageFormatQcow2,
		FirmwareType: store.FirmwareTypeUefi, DefaultCpuCores: 2, DefaultMemoryMib: 2048, DefaultDiskGib: 20,
	}); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	return nodeID, poolName, templateName
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

func TestVMCreateAttachesNicToNetwork(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	nodeID, poolName, templateName := schedulableFixtureWithNode(t, h, adminID)
	net := bridgeNetwork(t, h, admin)
	// The network-aware filter requires the requested network to be ready
	// on the candidate node before placement admits it.
	if err := h.store.UpsertNetworkNodeStatus(context.Background(), store.UpsertNetworkNodeStatusParams{
		NetworkID: uuid.MustParse(net.ID), NodeID: nodeID, ReconciliationStatus: "ready",
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus: %v", err)
	}

	resp := h.post(t, "/v1/vms", map[string]any{
		"name": "vm-" + uuid.NewString()[:8], "template": templateName, "pool": poolName,
		"vcpus": 2, "memory_mb": 2048, "network": net.Name,
	}, admin)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create vm status = %d, want 202", resp.StatusCode)
	}
	var accepted vmCreateAcceptedView
	decodeJSON(t, resp, &accepted)

	// The task names the VM as its resource; resolve the VM and assert a NIC.
	task, err := h.store.TaskByID(context.Background(), uuid.MustParse(accepted.TaskID))
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if task.ResourceID == nil {
		t.Fatal("task.ResourceID = nil, want vm id")
	}
	nics, err := h.store.ListVMNicsByVM(context.Background(), *task.ResourceID)
	if err != nil {
		t.Fatalf("ListVMNicsByVM: %v", err)
	}
	if len(nics) != 1 {
		t.Fatalf("nics = %d, want 1", len(nics))
	}
	netID := uuid.MustParse(net.ID)
	if nics[0].NetworkID != netID {
		t.Errorf("nic network = %v, want %v", nics[0].NetworkID, netID)
	}
	if nics[0].Model != store.NicModelVirtio {
		t.Errorf("nic model = %q, want virtio", nics[0].Model)
	}
	mac := nics[0].MacAddress.String()
	if len(mac) != 17 || mac[:8] != "52:54:00" {
		t.Errorf("nic mac = %q, want 52:54:00:xx:xx:xx", mac)
	}

	// The network now has an active referent: delete must be blocked.
	resp = h.delete(t, "/v1/networks/"+net.ID, admin)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete network status = %d, want 409 (blocked by nic)", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestVMCreateWithoutNetworkHasNoNic(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	poolName, templateName := schedulableFixture(t, h, adminID)

	resp := h.post(t, "/v1/vms", map[string]any{
		"name": "vm-" + uuid.NewString()[:8], "template": templateName, "pool": poolName,
		"vcpus": 2, "memory_mb": 2048,
	}, admin)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create vm status = %d, want 202", resp.StatusCode)
	}
	var accepted vmCreateAcceptedView
	decodeJSON(t, resp, &accepted)
	task, err := h.store.TaskByID(context.Background(), uuid.MustParse(accepted.TaskID))
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	nics, err := h.store.ListVMNicsByVM(context.Background(), *task.ResourceID)
	if err != nil {
		t.Fatalf("ListVMNicsByVM: %v", err)
	}
	if len(nics) != 0 {
		t.Errorf("nics = %d, want 0 (no --network)", len(nics))
	}
}

func TestVMCreateUnknownNetworkRejected(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	poolName, templateName := schedulableFixture(t, h, adminID)

	resp := h.post(t, "/v1/vms", map[string]any{
		"name": "vm-" + uuid.NewString()[:8], "template": templateName, "pool": poolName,
		"vcpus": 2, "memory_mb": 2048, "network": "does-not-exist",
	}, admin)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("create vm status = %d, want 404 (unknown network)", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestVMCreateNetworkFailedNoEligibleNode covers the network-aware
// placement filter (ADR 0034 NL18): the only schedulable node has the
// requested network in a `failed` reconciliation state, so placement
// must exclude it and the create returns 409 no_eligible_nodes with a
// network_not_ready reason rather than scheduling onto the bad node.
func TestVMCreateNetworkFailedNoEligibleNode(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	ctx := context.Background()
	s := h.store

	// Single ready node + pool + template, built inline so the test owns
	// the node id for the per-(node, network) status upsert.
	nodeID := uuid.New()
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
	poolName := "pool-" + uuid.NewString()[:8]
	if _, err := s.CreateStoragePool(ctx, store.CreateStoragePoolParams{
		ID: uuid.New(), NodeID: nodeID, Name: poolName, Type: "local_dir",
		Path: "/opt/otherix/pools/" + poolName, Config: []byte(`{}`),
	}); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}
	templateName := "tpl-" + uuid.NewString()[:8]
	if _, err := s.CreateTemplate(ctx, store.CreateTemplateParams{
		ID: uuid.New(), OwnerID: adminID, Name: templateName, Description: "e2e",
		Architecture: store.CpuArchAmd64, OsFamily: store.OsFamilyLinux, OsVariant: "noble",
		ImageUrl: "https://example.test/img.qcow2", ImageFormat: store.ImageFormatQcow2,
		FirmwareType: store.FirmwareTypeUefi, DefaultCpuCores: 2, DefaultMemoryMib: 2048, DefaultDiskGib: 20,
	}); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	net := bridgeNetwork(t, h, admin)
	// Mark the network failed-to-reconcile on the only candidate node.
	if err := s.UpsertNetworkNodeStatus(ctx, store.UpsertNetworkNodeStatusParams{
		NetworkID: uuid.MustParse(net.ID), NodeID: nodeID, ReconciliationStatus: "failed",
	}); err != nil {
		t.Fatalf("UpsertNetworkNodeStatus: %v", err)
	}

	resp := h.post(t, "/v1/vms", map[string]any{
		"name": "vm-" + uuid.NewString()[:8], "template": templateName, "pool": poolName,
		"vcpus": 2, "memory_mb": 2048, "network": net.Name,
	}, admin)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("create vm status = %d, want 409 (network failed on only node)", resp.StatusCode)
	}
	var env struct {
		Error struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	decodeJSON(t, resp, &env)
	if env.Error.Code != "no_eligible_nodes" {
		t.Errorf("error.code = %q, want no_eligible_nodes", env.Error.Code)
	}
	if reason, _ := env.Error.Details["reason"].(string); reason != "network_not_ready" {
		t.Errorf("details.reason = %q, want network_not_ready (details=%v)", reason, env.Error.Details)
	}
}
