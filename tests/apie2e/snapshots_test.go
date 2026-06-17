// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package apie2e

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentmock"
	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/api/handlers/snapshots"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// snapshotView is the apie2e projection of the snapshot HTTP responses - only
// the fields the contract tests assert on.
type snapshotView struct {
	ID            string `json:"id"`
	VMID          string `json:"vm_id"`
	OwnerID       string `json:"owner_id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	WithMemory    bool   `json:"with_memory"`
	DiskSizeBytes *int64 `json:"disk_size_bytes"`
}

type snapshotListResponse struct {
	Data []snapshotView `json:"data"`
}

type errorEnvelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// seedOwnedVM seeds a minimal scheduled VM owned by ownerID and returns its
// name + id. The snapshot create handler resolves the VM by name and reads its
// observed runtime phase for vm_state_at_snapshot; no vm_disks row is needed for
// the HTTP-contract assertions (the worker, not asserted here, produces blobs).
func seedOwnedVM(t *testing.T, s *etcdstore.Store, ownerID uuid.UUID) (vmName string, vmID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	spec, err := store.MarshalSchedulingSpec(store.SchedulingSpec{PoolName: "default"})
	if err != nil {
		t.Fatalf("MarshalSchedulingSpec: %v", err)
	}
	vm := store.VM{
		ID: uuid.New(), OwnerID: ownerID, Name: "snap-vm-" + uuid.NewString()[:8],
		DesiredPhase: store.VmDesiredPhaseRunning, Architecture: store.CpuArchAmd64,
		SchedulingStatus: store.VMSchedulingUnscheduled, SchedulingSpec: spec,
		CpuCores: 2, MemoryMib: 2048,
		Labels: []byte(`{}`), ImageFormat: store.ImageFormatQcow2,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	cli := sharedEtcdClient
	if err := cli.PutJSON(ctx, etcd.Key("vms", vm.ID.String()), vm); err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("uniq", "vms", "name", vm.Name), []byte(vm.ID.String())); err != nil {
		t.Fatalf("seed vm name guard: %v", err)
	}
	return vm.Name, vm.ID
}

// snapshotIDForVM resolves the (single) snapshot created for vmID via the store,
// so the test can poll GET /v1/snapshots/{id} without depending on the 202
// envelope carrying the snapshot id (it carries only the task id).
func snapshotIDForVM(t *testing.T, s *etcdstore.Store, vmID uuid.UUID) uuid.UUID {
	t.Helper()
	rows, err := s.ListSnapshots(context.Background(), store.ListSnapshotsParams{
		VmID: &vmID, LimitCount: 10,
	})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("snapshots for vm %s = %d, want 1", vmID, len(rows))
	}
	return rows[0].ID
}

// TestSnapshotCreate_202AndOwnership locks the create contract: a developer
// snapshotting their OWN vm gets 202 + a pending task and a creating row; a
// different developer gets 404 (no existence leak); with_memory:true is rejected
// 400 with details.reason=with_memory_unsupported.
func TestSnapshotCreate_202AndOwnership(t *testing.T) {
	h := newE2E(t)
	devToken, devID := loginAs(t, h, auth.RoleDeveloper)
	otherToken, _ := loginAs(t, h, auth.RoleDeveloper)

	vmName, vmID := seedOwnedVM(t, h.store, devID)

	// Owner create -> 202 + pending task.
	resp := h.do(t, http.MethodPost, "/v1/vms/"+vmName+"/snapshots",
		map[string]any{"name": "daily"}, devToken, map[string]string{
			"Idempotency-Key": "snap-create-" + uuid.NewString(),
		})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202", resp.StatusCode)
	}
	var accepted asyncAccepted
	decodeJSON(t, resp, &accepted)
	if accepted.Status != string(store.TaskStatusPending) {
		t.Errorf("accepted.status = %q, want pending", accepted.Status)
	}
	if accepted.TaskID == "" {
		t.Error("accepted.task_id is empty")
	}

	// The snapshot row exists in status=creating.
	sid := snapshotIDForVM(t, h.store, vmID)
	got, err := h.store.SnapshotByID(context.Background(), sid)
	if err != nil {
		t.Fatalf("SnapshotByID: %v", err)
	}
	if got.Status != store.SnapshotStatusCreating {
		t.Errorf("snapshot status = %q, want creating", got.Status)
	}
	if got.OwnerID != devID {
		t.Errorf("snapshot owner = %s, want %s", got.OwnerID, devID)
	}

	// A DIFFERENT developer creating a snapshot of that VM -> 404 (no leak).
	resp = h.do(t, http.MethodPost, "/v1/vms/"+vmName+"/snapshots",
		map[string]any{"name": "sneaky"}, otherToken, map[string]string{
			"Idempotency-Key": "snap-create-other-" + uuid.NewString(),
		})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("cross-owner create status = %d, want 404", resp.StatusCode)
	}
	var env errorEnvelope
	decodeJSON(t, resp, &env)
	// Cross-owner VM resolution returns the VM 404 envelope (vm_not_found),
	// shared with the genuinely-absent case so existence is not leaked.
	if env.Error.Code != "vm_not_found" {
		t.Errorf("cross-owner create code = %q, want vm_not_found", env.Error.Code)
	}

	// with_memory:true -> 400 validation_failed (details.reason=with_memory_unsupported).
	resp = h.do(t, http.MethodPost, "/v1/vms/"+vmName+"/snapshots",
		map[string]any{"name": "ram", "with_memory": true}, devToken, map[string]string{
			"Idempotency-Key": "snap-create-ram-" + uuid.NewString(),
		})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("with_memory create status = %d, want 400", resp.StatusCode)
	}
	var memEnv errorEnvelope
	decodeJSON(t, resp, &memEnv)
	if memEnv.Error.Code != "validation_failed" {
		t.Errorf("with_memory code = %q, want validation_failed", memEnv.Error.Code)
	}
	if got, ok := memEnv.Error.Details["reason"].(string); !ok || got != "with_memory_unsupported" {
		t.Errorf("with_memory details.reason = %v, want with_memory_unsupported", memEnv.Error.Details["reason"])
	}
}

// TestSnapshotList_OwnVisibilityAnd404 locks the list contract: the owner lists
// /v1/vms/{id}/snapshots and sees their snapshot; a non-owner GET on that VM's
// snapshots -> 404 (no existence leak).
func TestSnapshotList_OwnVisibilityAnd404(t *testing.T) {
	h := newE2E(t)
	devToken, devID := loginAs(t, h, auth.RoleDeveloper)
	otherToken, _ := loginAs(t, h, auth.RoleDeveloper)

	vmName, vmID := seedOwnedVM(t, h.store, devID)

	// Seed one snapshot directly via the store (owned by devID).
	sid := uuid.New()
	if _, err := h.store.CreateSnapshot(context.Background(), store.CreateSnapshotParams{
		ID: sid, VmID: vmID, OwnerID: devID, Name: "s1",
		VMStateAtSnapshot: store.VmStateAtSnapshotStopped,
		Task: store.CreateTaskParams{
			ID: uuid.New(), Type: "vm.snapshot.create", Status: store.TaskStatusPending,
			ResourceType: "snapshot", ResourceID: &sid, MaxAttempts: 25, CreatedBy: &devID,
		},
	}, fakeJobArgs{}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Owner list -> 200 with the snapshot visible.
	resp := h.get(t, "/v1/vms/"+vmName+"/snapshots", devToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner list status = %d, want 200", resp.StatusCode)
	}
	var list snapshotListResponse
	decodeJSON(t, resp, &list)
	if len(list.Data) != 1 || list.Data[0].ID != sid.String() {
		t.Errorf("owner list = %+v, want one snapshot %s", list.Data, sid)
	}

	// Non-owner list -> 404 (no existence leak).
	resp = h.get(t, "/v1/vms/"+vmName+"/snapshots", otherToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("non-owner list status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestSnapshotGet_ByID locks GET /v1/snapshots/{id}: returns the snapshot for
// the owner; a non-owner -> 404.
func TestSnapshotGet_ByID(t *testing.T) {
	h := newE2E(t)
	devToken, devID := loginAs(t, h, auth.RoleDeveloper)
	otherToken, _ := loginAs(t, h, auth.RoleDeveloper)

	_, vmID := seedOwnedVM(t, h.store, devID)

	sid := uuid.New()
	if _, err := h.store.CreateSnapshot(context.Background(), store.CreateSnapshotParams{
		ID: sid, VmID: vmID, OwnerID: devID, Name: "s1",
		VMStateAtSnapshot: store.VmStateAtSnapshotStopped,
		Task: store.CreateTaskParams{
			ID: uuid.New(), Type: "vm.snapshot.create", Status: store.TaskStatusPending,
			ResourceType: "snapshot", ResourceID: &sid, MaxAttempts: 25, CreatedBy: &devID,
		},
	}, fakeJobArgs{}); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}

	// Owner get -> 200.
	resp := h.get(t, "/v1/snapshots/"+sid.String(), devToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner get status = %d, want 200", resp.StatusCode)
	}
	var view snapshotView
	decodeJSON(t, resp, &view)
	if view.ID != sid.String() || view.VMID != vmID.String() {
		t.Errorf("owner get = %+v, want snapshot %s of vm %s", view, sid, vmID)
	}

	// Non-owner get -> 404.
	resp = h.get(t, "/v1/snapshots/"+sid.String(), otherToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("non-owner get status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestSnapshotDelete_FailsClosedChildren locks the fail-closed delete: a
// snapshot with a non-deleted child -> DELETE /v1/snapshots/{id} returns 409
// conflict with details.reason=snapshot_has_children.
func TestSnapshotDelete_FailsClosedChildren(t *testing.T) {
	h := newE2E(t)
	devToken, devID := loginAs(t, h, auth.RoleDeveloper)

	_, vmID := seedOwnedVM(t, h.store, devID)

	parentID := uuid.New()
	if _, err := h.store.CreateSnapshot(context.Background(), store.CreateSnapshotParams{
		ID: parentID, VmID: vmID, OwnerID: devID, Name: "parent",
		VMStateAtSnapshot: store.VmStateAtSnapshotStopped,
		Task: store.CreateTaskParams{
			ID: uuid.New(), Type: "vm.snapshot.create", Status: store.TaskStatusPending,
			ResourceType: "snapshot", ResourceID: &parentID, MaxAttempts: 25, CreatedBy: &devID,
		},
	}, fakeJobArgs{}); err != nil {
		t.Fatalf("CreateSnapshot parent: %v", err)
	}

	// Seed a child snapshot whose ParentSnapshotID == parentID directly (the
	// store has no API to set a parent; write the row + per-VM index entry).
	childID := uuid.New()
	now := time.Now().UTC()
	child := store.Snapshot{
		ID: childID, VmID: vmID, OwnerID: devID, ParentSnapshotID: &parentID,
		Name: "child", Status: store.SnapshotStatusReady,
		VMStateAtSnapshot: store.VmStateAtSnapshotStopped,
		CreatedAt:         now, UpdatedAt: now,
	}
	cli := sharedEtcdClient
	if err := cli.PutJSON(context.Background(), etcd.Key("snapshots", childID.String()), child); err != nil {
		t.Fatalf("seed child snapshot: %v", err)
	}
	if err := cli.Put(context.Background(), etcd.Key("index", "snapshots", "vm", vmID.String(), childID.String()), []byte(childID.String())); err != nil {
		t.Fatalf("seed child vm index: %v", err)
	}

	// Delete the parent -> 409 conflict snapshot_has_children.
	resp := h.do(t, http.MethodDelete, "/v1/snapshots/"+parentID.String(), nil, devToken, map[string]string{
		"Idempotency-Key": "snap-del-" + uuid.NewString(),
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("delete-with-children status = %d, want 409", resp.StatusCode)
	}
	var env errorEnvelope
	decodeJSON(t, resp, &env)
	if env.Error.Code != "conflict" {
		t.Errorf("delete code = %q, want conflict", env.Error.Code)
	}
	if got, ok := env.Error.Details["reason"].(string); !ok || got != "snapshot_has_children" {
		t.Errorf("delete details.reason = %v, want snapshot_has_children", env.Error.Details["reason"])
	}
}

// TestSnapshotCreate_RecordsSourceArchAndFirmware locks the manifest-completeness
// contract: a snapshot created of a VM records that VM's architecture and
// firmware_id at capture time, so recreate-from-snapshot is self-describing. The
// source VM here carries an explicit firmware id and arm64 architecture.
func TestSnapshotCreate_RecordsSourceArchAndFirmware(t *testing.T) {
	h := newE2E(t)
	devToken, devID := loginAs(t, h, auth.RoleDeveloper)
	ctx := context.Background()

	fwID := uuid.New()
	// Seed a VM with arm64 + an explicit firmware id directly so the snapshot
	// handler reads them off the resolved VM row.
	spec, err := store.MarshalSchedulingSpec(store.SchedulingSpec{PoolName: "default"})
	if err != nil {
		t.Fatalf("MarshalSchedulingSpec: %v", err)
	}
	vm := store.VM{
		ID: uuid.New(), OwnerID: devID, Name: "snap-arm-" + uuid.NewString()[:8],
		DesiredPhase: store.VmDesiredPhaseRunning, Architecture: store.CpuArchArm64,
		FirmwareID:       &fwID,
		SchedulingStatus: store.VMSchedulingUnscheduled, SchedulingSpec: spec,
		CpuCores: 2, MemoryMib: 2048, Labels: []byte(`{}`), ImageFormat: store.ImageFormatQcow2,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	cli := sharedEtcdClient
	if err := cli.PutJSON(ctx, etcd.Key("vms", vm.ID.String()), vm); err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("uniq", "vms", "name", vm.Name), []byte(vm.ID.String())); err != nil {
		t.Fatalf("seed vm name guard: %v", err)
	}

	resp := h.do(t, http.MethodPost, "/v1/vms/"+vm.Name+"/snapshots",
		map[string]any{"name": "captured"}, devToken, map[string]string{
			"Idempotency-Key": "snap-arch-" + uuid.NewString(),
		})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()

	sid := snapshotIDForVM(t, h.store, vm.ID)
	got, err := h.store.SnapshotByID(ctx, sid)
	if err != nil {
		t.Fatalf("SnapshotByID: %v", err)
	}
	if got.SourceArchitecture != store.CpuArchArm64 {
		t.Errorf("snapshot SourceArchitecture = %q, want arm64", got.SourceArchitecture)
	}
	if got.SourceFirmwareID == nil || *got.SourceFirmwareID != fwID {
		t.Errorf("snapshot SourceFirmwareID = %v, want %s", got.SourceFirmwareID, fwID)
	}
}

// fakeJobArgs is a no-op queue.JobArgs for store-seeded snapshots in the list /
// get / delete tests (the worker is not driven in apie2e).
type fakeJobArgs struct{}

func (fakeJobArgs) Kind() string { return "vm.snapshot.create" }

// newMockAgentClient builds a real agentclient.Client over the agentmock TLS
// material (CP-side leaf cert + cluster CA), so the snapshot worker drives the
// SAME production agent_executor.go decode path against the mock that it would
// against a real agent. Fast poll intervals keep the end-to-end loop sub-second.
func newMockAgentClient(t *testing.T) *agentclient.Client {
	t.Helper()
	dir := t.TempDir()
	caPEM, err := agentmock.CACertPEM()
	if err != nil {
		t.Fatalf("agentmock.CACertPEM: %v", err)
	}
	cpCertPEM, err := agentmock.ControlPlaneCertPEM()
	if err != nil {
		t.Fatalf("agentmock.ControlPlaneCertPEM: %v", err)
	}
	cpKeyPEM, err := agentmock.ControlPlaneKeyPEM()
	if err != nil {
		t.Fatalf("agentmock.ControlPlaneKeyPEM: %v", err)
	}
	for _, w := range []struct {
		name  string
		bytes []byte
	}{
		{"ca.crt", caPEM},
		{"tls.crt", cpCertPEM},
		{"tls.key", cpKeyPEM},
	} {
		if err := os.WriteFile(filepath.Join(dir, w.name), w.bytes, 0o600); err != nil {
			t.Fatalf("write %s: %v", w.name, err)
		}
	}
	cert, ca, err := agentclient.LoadMaterialFromFiles(
		filepath.Join(dir, "tls.crt"),
		filepath.Join(dir, "tls.key"),
		filepath.Join(dir, "ca.crt"),
	)
	if err != nil {
		t.Fatalf("LoadMaterialFromFiles: %v", err)
	}
	cli, err := agentclient.New(config.AgentClientConfig{
		Enabled:         true,
		Timeout:         5 * time.Second,
		PollInterval:    10 * time.Millisecond,
		PollMaxInterval: 200 * time.Millisecond,
	}, cert, ca)
	if err != nil {
		t.Fatalf("agentclient.New: %v", err)
	}
	return cli
}

// seedRunningVMOnNode seeds a VM owned by ownerID with a vm_runtime row pinned to
// node, so the snapshot worker resolves snapshot -> VM -> node and reaches the
// agent at node.AdvertisedEndpoint. The node's advertised endpoint is the mock
// agent's TLS URL.
func seedRunningVMOnNode(t *testing.T, s *etcdstore.Store, ownerID uuid.UUID, agentURL string) (vmName string, vmID, nodeID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	node, err := s.CreateNode(ctx, store.CreateNodeParams{
		ID:                      uuid.New(),
		Name:                    "snap-node-" + uuid.NewString()[:8],
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      agentURL,
		MigrationHost:           "10.0.0.7",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  store.NodeStatusReady,
	})
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	spec, err := store.MarshalSchedulingSpec(store.SchedulingSpec{PoolName: "default"})
	if err != nil {
		t.Fatalf("MarshalSchedulingSpec: %v", err)
	}
	pinned := node.ID
	vm := store.VM{
		ID: uuid.New(), OwnerID: ownerID, Name: "snap-run-" + uuid.NewString()[:8],
		DesiredPhase: store.VmDesiredPhaseRunning, Architecture: store.CpuArchAmd64,
		SchedulingStatus: store.VMSchedulingScheduled, SchedulingSpec: spec,
		PinnedNodeID: &pinned,
		CpuCores:     2, MemoryMib: 2048,
		Labels: []byte(`{}`), ImageFormat: store.ImageFormatQcow2,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	cli := sharedEtcdClient
	if err := cli.PutJSON(ctx, etcd.Key("vms", vm.ID.String()), vm); err != nil {
		t.Fatalf("seed vm: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("uniq", "vms", "name", vm.Name), []byte(vm.ID.String())); err != nil {
		t.Fatalf("seed vm name guard: %v", err)
	}
	// A vm_runtime row pinned to the node is what resolveSnapshotNode reads to
	// find the agent endpoint - without it the worker fails with no-current-node.
	if err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		return hp.UpsertVMRuntime(ctx, store.UpsertVMRuntimeParams{
			VmID: vm.ID, CurrentNodeID: &node.ID, Phase: store.VmPhaseRunning, ObservedGeneration: 1,
		})
	}); err != nil {
		t.Fatalf("UpsertVMRuntime: %v", err)
	}
	return vm.Name, vm.ID, node.ID
}

// TestSnapshotCreate_EndToEnd_ReachesReady drives the FULL CP seam against the
// mock agent: a developer snapshots their own VM (202 + create task), the
// snapshot worker POSTs to the mock agent and polls the agent task to terminal,
// projects the mock's deterministic two-disk content-addressed manifest, and the
// snapshot row reaches status=ready. This is the seam that historically passed on
// mock but 404'd on real agent - the mock now speaks the new content-addressed
// contract through the SAME agent_executor.go decode path the real agent will use.
func TestSnapshotCreate_EndToEnd_ReachesReady(t *testing.T) {
	h := newE2E(t)
	devToken, devID := loginAs(t, h, auth.RoleDeveloper)
	ctx := context.Background()

	mock := agentmock.Start(t, agentmock.Options{TLS: agentmock.TLSEnabled, Architecture: "amd64"})

	vmName, vmID, _ := seedRunningVMOnNode(t, h.store, devID, mock.URL())

	// Owner create -> 202 + pending task carrying the create task id.
	resp := h.do(t, http.MethodPost, "/v1/vms/"+vmName+"/snapshots",
		map[string]any{"name": "daily"}, devToken, map[string]string{
			"Idempotency-Key": "snap-e2e-" + uuid.NewString(),
		})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d, want 202", resp.StatusCode)
	}
	var accepted asyncAccepted
	decodeJSON(t, resp, &accepted)
	taskID, err := uuid.Parse(accepted.TaskID)
	if err != nil {
		t.Fatalf("parse accepted.task_id %q: %v", accepted.TaskID, err)
	}
	sid := snapshotIDForVM(t, h.store, vmID)

	// Drive the snapshot worker against the mock agent through the production
	// agent_executor.go: PostSnapshot -> PollTask -> decodeSnapshotResult ->
	// SnapshotManifestApplied -> finalize task success.
	exec := snapshots.NewAgentSnapshotExecutor(newMockAgentClient(t))
	handler := snapshots.CreateHandler(h.store, exec, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	rawArgs, err := json.Marshal(snapshots.SnapshotCreateArgs{TaskID: taskID, SnapshotID: sid})
	if err != nil {
		t.Fatalf("marshal SnapshotCreateArgs: %v", err)
	}
	if err := handler(ctx, rawArgs); err != nil {
		t.Fatalf("snapshot create worker: %v", err)
	}

	// The backing task reached success.
	task, err := h.store.TaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskByID: %v", err)
	}
	if task.Status != store.TaskStatusSuccess {
		t.Fatalf("task status = %q, want success", task.Status)
	}

	// The snapshot row reached ready with the mock's deterministic two-disk
	// content-addressed manifest and vm_state_at_snapshot set.
	got, err := h.store.SnapshotByID(ctx, sid)
	if err != nil {
		t.Fatalf("SnapshotByID: %v", err)
	}
	if got.Status != store.SnapshotStatusReady {
		t.Fatalf("snapshot status = %q, want ready", got.Status)
	}
	if got.VMStateAtSnapshot != store.VmStateAtSnapshotStopped {
		t.Errorf("vm_state_at_snapshot = %q, want stopped", got.VMStateAtSnapshot)
	}
	if len(got.Disks) != 2 {
		t.Fatalf("manifest disks = %d, want 2", len(got.Disks))
	}
	wantSHA := map[string]string{"virtio0": mockSnapshotDiskSHA0, "virtio1": mockSnapshotDiskSHA1}
	for _, d := range got.Disks {
		if want, ok := wantSHA[d.Device]; !ok || d.SHA256 != want {
			t.Errorf("disk %q sha256 = %q, want %q", d.Device, d.SHA256, want)
		}
		if d.SizeBytes <= 0 {
			t.Errorf("disk %q size_bytes = %d, want > 0", d.Device, d.SizeBytes)
		}
	}
}

// mockSnapshotDiskSHA0/SHA1 are the deterministic fixed digests the mock agent
// reports for the two-disk content-addressed manifest. Mirrored here so the
// end-to-end test asserts wire-shape parity with the mock.
const (
	mockSnapshotDiskSHA0 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mockSnapshotDiskSHA1 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)
