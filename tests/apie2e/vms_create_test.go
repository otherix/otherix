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

// seedReadySnapshot seeds a snapshot of vmID owned by ownerID, recording the
// given source architecture, then flips it to status=ready via the manifest-apply
// path so recreate-from-snapshot can consume it. It returns the snapshot id.
func seedReadySnapshot(t *testing.T, s *etcdstore.Store, vmID, ownerID uuid.UUID, arch store.CPUArch) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	sid := uuid.New()
	if _, err := s.CreateSnapshot(ctx, store.CreateSnapshotParams{
		ID: sid, VmID: vmID, OwnerID: ownerID, Name: "s-" + uuid.NewString()[:8],
		VMStateAtSnapshot:  store.VmStateAtSnapshotStopped,
		SourceArchitecture: arch,
		Task: store.CreateTaskParams{
			ID: uuid.New(), Type: "vm.snapshot.create", Status: store.TaskStatusPending,
			ResourceType: "snapshot", ResourceID: &sid, MaxAttempts: 25, CreatedBy: &ownerID,
		},
	}, fakeJobArgs{}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if err := s.SnapshotManifestApplied(ctx, sid, uuid.New(), []store.SnapshotDisk{
		{Index: 0, Device: "virtio0", SHA256: "deadbeef", SizeBytes: 1 << 20, Format: "qcow2"},
	}, store.VmStateAtSnapshotStopped); err != nil {
		t.Fatalf("SnapshotManifestApplied: %v", err)
	}
	return sid
}

// TestVMCreate_OneOfImageOrSnapshot locks the exactly-one-of-image-or-snapshot
// admission contract through the real HTTP edge:
//
//   - both image_url and source_snapshot_id set -> 400 image_xor_snapshot.
//   - neither set -> 400 image_xor_snapshot.
//   - source_snapshot_id pointing at a ready, owned snapshot -> 201 pending, and
//     vm get shows source_snapshot_id with empty image_url; architecture comes
//     from the snapshot manifest.
//   - source_snapshot_id pointing at a creating (not-ready) snapshot -> 409
//     snapshot_not_ready.
func TestVMCreate_OneOfImageOrSnapshot(t *testing.T) {
	h := newE2E(t)
	admin, adminID := loginAs(t, h, auth.RoleAdmin)
	poolName := schedulableFixture(t, h, adminID)

	// Both image_url and source_snapshot_id -> 400 image_xor_snapshot.
	both := vmCreateBody(map[string]any{"pool": poolName, "source_snapshot_id": uuid.NewString()})
	resp := h.post(t, "/v1/vms", both, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("both-set create status = %d, want 400", resp.StatusCode)
	}
	var bothEnv errorEnvelope
	decodeJSON(t, resp, &bothEnv)
	if bothEnv.Error.Code != "validation_failed" {
		t.Errorf("both-set code = %q, want validation_failed", bothEnv.Error.Code)
	}
	if got, _ := bothEnv.Error.Details["reason"].(string); got != "image_xor_snapshot" {
		t.Errorf("both-set details.reason = %v, want image_xor_snapshot", bothEnv.Error.Details["reason"])
	}

	// Neither image_url nor source_snapshot_id -> 400 image_xor_snapshot.
	neither := map[string]any{
		"name": "vm-" + uuid.NewString()[:8], "arch": "amd64",
		"vcpus": 2, "memory_mb": 2048, "pool": poolName,
	}
	resp = h.post(t, "/v1/vms", neither, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("neither-set create status = %d, want 400", resp.StatusCode)
	}
	var neitherEnv errorEnvelope
	decodeJSON(t, resp, &neitherEnv)
	if got, _ := neitherEnv.Error.Details["reason"].(string); got != "image_xor_snapshot" {
		t.Errorf("neither-set details.reason = %v, want image_xor_snapshot", neitherEnv.Error.Details["reason"])
	}

	// Snapshot-sourced create against a ready, owned snapshot -> 201 pending.
	_, vmID := seedOwnedVM(t, h.store, adminID)
	readyID := seedReadySnapshot(t, h.store, vmID, adminID, store.CpuArchArm64)
	body := map[string]any{
		"name":  "vm-restored-" + uuid.NewString()[:8],
		"vcpus": 2, "memory_mb": 2048, "pool": poolName,
		"source_snapshot_id": readyID.String(),
	}
	vmName := body["name"].(string)
	resp = h.post(t, "/v1/vms", body, admin)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("snapshot-sourced create status = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	resp = h.get(t, "/v1/vms/"+vmName, admin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get restored vm status = %d, want 200", resp.StatusCode)
	}
	var view struct {
		ImageURL         string  `json:"image_url"`
		Architecture     string  `json:"architecture"`
		SourceSnapshotID *string `json:"source_snapshot_id"`
	}
	decodeJSON(t, resp, &view)
	if view.SourceSnapshotID == nil || *view.SourceSnapshotID != readyID.String() {
		t.Errorf("source_snapshot_id = %v, want %s", view.SourceSnapshotID, readyID)
	}
	if view.ImageURL != "" {
		t.Errorf("image_url = %q, want empty (snapshot-sourced)", view.ImageURL)
	}
	if view.Architecture != "arm64" {
		t.Errorf("architecture = %q, want arm64 (from snapshot manifest)", view.Architecture)
	}

	// Snapshot-sourced create that ALSO supplies architecture -> 400
	// field_from_snapshot (arch/firmware come from the manifest, not the request).
	withArch := map[string]any{
		"name": "vm-" + uuid.NewString()[:8], "arch": "amd64",
		"vcpus": 2, "memory_mb": 2048, "pool": poolName,
		"source_snapshot_id": readyID.String(),
	}
	resp = h.post(t, "/v1/vms", withArch, admin)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("snapshot+arch create status = %d, want 400", resp.StatusCode)
	}
	var faEnv errorEnvelope
	decodeJSON(t, resp, &faEnv)
	if got, _ := faEnv.Error.Details["reason"].(string); got != "field_from_snapshot" {
		t.Errorf("snapshot+arch details.reason = %v, want field_from_snapshot", faEnv.Error.Details["reason"])
	}

	// Snapshot-sourced create against a creating (not-ready) snapshot -> 409.
	creatingID := uuid.New()
	if _, err := h.store.CreateSnapshot(context.Background(), store.CreateSnapshotParams{
		ID: creatingID, VmID: vmID, OwnerID: adminID, Name: "still-creating",
		VMStateAtSnapshot:  store.VmStateAtSnapshotStopped,
		SourceArchitecture: store.CpuArchAmd64,
		Task: store.CreateTaskParams{
			ID: uuid.New(), Type: "vm.snapshot.create", Status: store.TaskStatusPending,
			ResourceType: "snapshot", ResourceID: &creatingID, MaxAttempts: 25, CreatedBy: &adminID,
		},
	}, fakeJobArgs{}); err != nil {
		t.Fatalf("CreateSnapshot (creating): %v", err)
	}
	notReady := map[string]any{
		"name":  "vm-" + uuid.NewString()[:8],
		"vcpus": 2, "memory_mb": 2048, "pool": poolName,
		"source_snapshot_id": creatingID.String(),
	}
	resp = h.post(t, "/v1/vms", notReady, admin)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("not-ready snapshot create status = %d, want 409", resp.StatusCode)
	}
	var nrEnv errorEnvelope
	decodeJSON(t, resp, &nrEnv)
	if nrEnv.Error.Code != "conflict" {
		t.Errorf("not-ready code = %q, want conflict", nrEnv.Error.Code)
	}
	if got, _ := nrEnv.Error.Details["reason"].(string); got != "snapshot_not_ready" {
		t.Errorf("not-ready details.reason = %v, want snapshot_not_ready", nrEnv.Error.Details["reason"])
	}
}

// TestVMCreate_ArtifactPoolRejected locks role separation: vm create against an
// artifact pool name returns 409 pool_role_invalid (artifact pools cannot host
// VM disks).
func TestVMCreate_ArtifactPoolRejected(t *testing.T) {
	h := newE2E(t)
	admin, _ := loginAs(t, h, auth.RoleAdmin)

	h.post(t, "/v1/artifact-pools", map[string]any{"name": "gold", "replication_factor": 1}, admin).Body.Close()

	body := vmCreateBody(map[string]any{"pool": "gold"})
	resp := h.post(t, "/v1/vms", body, admin)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	var env errorEnvelope
	decodeJSON(t, resp, &env)
	if env.Error.Code != "pool_role_invalid" {
		t.Errorf("code = %q, want pool_role_invalid", env.Error.Code)
	}
}

// TestVMCreate_FromSnapshot_CrossOwner404 locks the cross-user provenance
// boundary: a caller who holds snapshot:read but does not own the snapshot must
// see 404 (not 403) when trying to recreate from it - existence is not leaked.
func TestVMCreate_FromSnapshot_CrossOwner404(t *testing.T) {
	h := newE2E(t)
	_, adminID := loginAs(t, h, auth.RoleAdmin)
	poolName := schedulableFixture(t, h, adminID)
	devToken, devID := loginAs(t, h, auth.RoleDeveloper)

	// A snapshot owned by admin, ready.
	_, vmID := seedOwnedVM(t, h.store, adminID)
	readyID := seedReadySnapshot(t, h.store, vmID, adminID, store.CpuArchAmd64)

	// A different user (developer) recreates from it -> 404 (no existence leak).
	body := map[string]any{
		"name":  "vm-" + uuid.NewString()[:8],
		"vcpus": 2, "memory_mb": 2048, "pool": poolName,
		"source_snapshot_id": readyID.String(),
	}
	resp := h.post(t, "/v1/vms", body, devToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-owner recreate status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	_ = devID
}
