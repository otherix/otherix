// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// vmView is the subset of the vms.get projection the image-model tests assert
// on: the self-describing image fields plus the resource id.
type vmView struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ImageURL     string `json:"image_url"`
	ImageSHA256  string `json:"image_sha256"`
	Format       string `json:"format"`
	Architecture string `json:"architecture"`
}

// TestVMCreate_ReturnsPendingImmediately is the admission-only contract: a VM
// create returns 201 + the VM view with status.phase=pending immediately, even
// when no pool exists yet (placement is deferred to the vms.schedule loop). A
// duplicate name still fails fast with 409.
func TestVMCreate_ReturnsPendingImmediately(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	// No pool seeded yet -> create must still succeed as pending.
	body := vmCreateBody(map[string]any{"pool": "default"})
	resp := h.post(t, "/v1/vms", body, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/vms = %d, want 201", resp.StatusCode)
	}
	var vm struct {
		Name   string `json:"name"`
		Status struct {
			Phase  string `json:"phase"`
			Reason string `json:"reason"`
		} `json:"status"`
	}
	decodeJSON(t, resp, &vm)
	if vm.Status.Phase != "pending" {
		t.Errorf("status.phase = %q, want pending", vm.Status.Phase)
	}

	// Duplicate name -> 409.
	r2 := h.post(t, "/v1/vms", body, admin)
	if r2.StatusCode != http.StatusConflict {
		t.Errorf("dup create = %d, want 409", r2.StatusCode)
	}
	r2.Body.Close()
}

// TestVMCreateFromImageSurfacesImageFields drives the image-model create over
// the HTTP edge - no template - and asserts the synchronously-committed VM row,
// read back through vms.get, is self-describing: image_url, the pinned
// image_sha256, architecture, and format all surface on the public view.
func TestVMCreateFromImageSurfacesImageFields(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	poolName := schedulableFixture(t, h, adminID)

	const sha = "1111111111111111111111111111111111111111111111111111111111111111"
	body := vmCreateBody(map[string]any{"pool": poolName, "image_sha256": sha})
	vmName := body["name"].(string)
	resp := h.post(t, "/v1/vms", body, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create vm status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	// vms.get is name-keyed (UUIDs are rejected); read the VM back by the name
	// the create committed synchronously.
	resp = h.get(t, "/v1/vms/"+vmName, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get vm status = %d, want 200", resp.StatusCode)
	}
	var vm vmView
	decodeJSON(t, resp, &vm)

	if vm.ImageURL != imageURL {
		t.Errorf("image_url = %q, want %q", vm.ImageURL, imageURL)
	}
	if vm.ImageSHA256 != sha {
		t.Errorf("image_sha256 = %q, want %q", vm.ImageSHA256, sha)
	}
	if vm.Architecture != "amd64" {
		t.Errorf("architecture = %q, want amd64", vm.Architecture)
	}
	if vm.Format != "qcow2" {
		t.Errorf("format = %q, want qcow2 (default)", vm.Format)
	}
}

// TestDeveloperCreatesVMFromAnyImage is the RBAC assertion for the image model:
// vm:create is the single gate (ScopeAny for developer), with no template gate
// in the path. A developer creates a VM from an arbitrary image URL and the
// create is admitted (201 pending), not 403.
func TestDeveloperCreatesVMFromAnyImage(t *testing.T) {
	h := newE2E(t)
	// The pool + firmware must exist (admin-seeded), then the developer creates.
	_, adminID := loginAs(t, h, auth.RoleAdmin)
	poolName := schedulableFixture(t, h, adminID)
	dev, _ := loginAs(t, h, auth.RoleDeveloper)

	resp := h.post(t, "/v1/vms", vmCreateBody(map[string]any{
		"pool":      poolName,
		"image_url": "https://images.example.test/custom/dev-built.qcow2",
	}), dev)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("developer create vm status = %d, want 201 (vm:create is the only gate)", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestPoolGetSurfacesImageInventory asserts the agent-reported per-pool image
// inventory (the heartbeat `pools[].images[]` payload, persisted via
// UpsertPoolImageInventory) surfaces on the flat per-instance pool view as
// `images[]`.
func TestPoolGetSurfacesImageInventory(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)
	ctx := context.Background()
	s := h.store

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
	poolID := uuid.New()
	poolName := "pool-" + uuid.NewString()[:8]
	if _, err := s.CreateStoragePool(ctx, store.CreateStoragePoolParams{
		ID: poolID, NodeID: nodeID, Name: poolName, Type: "local_dir",
		Path: "/opt/otherix/pools/" + poolName, Config: []byte(`{}`),
	}); err != nil {
		t.Fatalf("CreateStoragePool: %v", err)
	}

	// Simulate the heartbeat projection: the agent reported one cached image.
	imported := time.Now().UTC().Truncate(time.Second)
	if err := s.UpsertPoolImageInventory(ctx, poolID, []store.PoolImage{{
		Basename: "noble.qcow2", ChecksumSha256: "abc123", SizeBytes: 1 << 20,
		VirtualSizeBytes: 2 << 30, Format: "qcow2", ImportedAt: imported,
	}}); err != nil {
		t.Fatalf("UpsertPoolImageInventory: %v", err)
	}

	resp := h.get(t, "/v1/storage-pools/"+poolID.String(), admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get pool status = %d, want 200", resp.StatusCode)
	}
	var pool struct {
		Images []struct {
			Name             string `json:"name"`
			SHA256           string `json:"sha256"`
			SizeBytes        int64  `json:"size_bytes"`
			VirtualSizeBytes int64  `json:"virtual_size_bytes"`
			Format           string `json:"format"`
		} `json:"images"`
	}
	decodeJSON(t, resp, &pool)

	want := []struct {
		Name             string `json:"name"`
		SHA256           string `json:"sha256"`
		SizeBytes        int64  `json:"size_bytes"`
		VirtualSizeBytes int64  `json:"virtual_size_bytes"`
		Format           string `json:"format"`
	}{{Name: "noble.qcow2", SHA256: "abc123", SizeBytes: 1 << 20, VirtualSizeBytes: 2 << 30, Format: "qcow2"}}
	if diff := cmp.Diff(want, pool.Images); diff != "" {
		t.Errorf("pool images mismatch (-want +got):\n%s", diff)
	}
}

// TestTemplateRoutesRemovedReturn404 confirms the template entity is gone from
// the router: both the list and create paths are unrouted, so chi returns 404.
func TestTemplateRoutesRemovedReturn404(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	resp := h.get(t, "/v1/templates", admin)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /v1/templates status = %d, want 404 (route removed)", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.post(t, "/v1/templates", map[string]any{"name": "x"}, admin)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST /v1/templates status = %d, want 404 (route removed)", resp.StatusCode)
	}
	resp.Body.Close()
}
