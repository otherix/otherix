// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
)

// Deterministic two-disk content-addressed manifest the mock reports for every
// snapshot create. The digests are fixed 64-char lowercase hex so end-to-end
// tests assert exact wire-shape parity; the sizes are deterministic non-zero
// values. The mock never hashes real bytes - it is a test double.
const (
	mockSnapshotDisk0SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mockSnapshotDisk1SHA256 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	mockSnapshotDisk0Size   = int64(512 << 20) // 512 MiB
	mockSnapshotDisk1Size   = int64(64 << 20)  // 64 MiB
)

// snapshotKey indexes the per-(vm name, snapshot name) record of which orphaned
// blob digests the CP asked the agent to GC.
type snapshotKey struct {
	VMName       string
	SnapshotName string
}

// mockSnapshot is the materialised manifest the mock list / get surface returns
// and the create-task terminal projection reports. Deterministic, no real qemu.
type mockSnapshot struct {
	vmName            string
	vmUUID            uuid.UUID
	name              string
	description       string
	architecture      string
	vmStateAtSnapshot string
	createdAt         time.Time
	disks             []mockSnapshotDisk
}

// mockSnapshotDisk is one content-addressed blob descriptor in the manifest.
type mockSnapshotDisk struct {
	index     int
	device    string
	sha256    string
	sizeBytes int64
}

// mockSnapshotManifest returns the deterministic two-disk manifest captured for
// vm at snapshot time: virtio0 / virtio1 with fixed digests and sizes, the VM's
// architecture, and vm_state_at_snapshot=stopped (slice A is disk-only, no RAM).
func mockSnapshotManifest(vm AgentVM, name, description string, now time.Time) mockSnapshot {
	arch := vm.Architecture
	if arch == "" {
		arch = "amd64"
	}
	return mockSnapshot{
		vmName:            vm.Name,
		vmUUID:            vm.ID,
		name:              name,
		description:       description,
		architecture:      arch,
		vmStateAtSnapshot: "stopped",
		createdAt:         now,
		disks: []mockSnapshotDisk{
			{index: 0, device: "virtio0", sha256: mockSnapshotDisk0SHA256, sizeBytes: mockSnapshotDisk0Size},
			{index: 1, device: "virtio1", sha256: mockSnapshotDisk1SHA256, sizeBytes: mockSnapshotDisk1Size},
		},
	}
}

// toAPI projects a mockSnapshot into the agentapi.Snapshot wire shape. This is
// the SAME shape the CP worker's agent_executor.go decodes (re-marshals the task
// result map and unmarshals into agentapi.Snapshot), so the mock proves the
// content-addressed contract end-to-end.
func (s mockSnapshot) toAPI() agentapi.Snapshot {
	disks := make([]struct {
		Device    string                       `json:"device"`
		Format    agentapi.SnapshotDisksFormat `json:"format"`
		Index     int                          `json:"index"`
		Sha256    string                       `json:"sha256"`
		SizeBytes int64                        `json:"size_bytes"`
	}, 0, len(s.disks))
	for _, d := range s.disks {
		disks = append(disks, struct {
			Device    string                       `json:"device"`
			Format    agentapi.SnapshotDisksFormat `json:"format"`
			Index     int                          `json:"index"`
			Sha256    string                       `json:"sha256"`
			SizeBytes int64                        `json:"size_bytes"`
		}{
			Device:    d.device,
			Format:    agentapi.SnapshotDisksFormatQcow2,
			Index:     d.index,
			Sha256:    d.sha256,
			SizeBytes: d.sizeBytes,
		})
	}
	out := agentapi.Snapshot{
		Architecture:      agentapi.SnapshotArchitecture(s.architecture),
		CreatedAt:         s.createdAt,
		Disks:             disks,
		Name:              s.name,
		Status:            agentapi.SnapshotStatusReady,
		VMStateAtSnapshot: agentapi.SnapshotVMStateAtSnapshot(s.vmStateAtSnapshot),
		VMUUID:            s.vmUUID,
	}
	if s.description != "" {
		desc := s.description
		out.Description = &desc
	}
	return out
}

// VMSnapshotsCreate implements POST /v1/vms/{vm_name}/snapshots. Decodes the
// create request, mints a snapshot-create agent task whose terminal-success
// projection reports the deterministic two-disk content-addressed manifest
// (decoded by the CP worker's agent_executor.go), and returns 202 +
// AsyncTaskAccepted. The manifest materialises into state.snapshots lazily on
// terminal-success (see materializeSnapshotCreateLocked), so the list / get
// surface observes it after the task settles.
func (m *Mock) VMSnapshotsCreate(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName, _ agentapi.VMSnapshotsCreateParams) {
	const opID = "vmSnapshots.create"
	if m.preDispatch(w, r, opID) {
		return
	}

	var body agentapi.SnapshotCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		m.respondError(w, r, opID, http.StatusBadRequest, "validation_failed", "malformed snapshot create request body")
		return
	}
	if body.Name == "" {
		m.respondError(w, r, opID, http.StatusBadRequest, "validation_failed", "name is required")
		return
	}

	agentTaskID := uuid.New()
	now := time.Now().UTC()
	description := ""
	if body.Description != nil {
		description = *body.Description
	}

	m.state.mu.Lock()
	vm, ok := m.state.storedVMs[vmName]
	if !ok {
		// The mock has no inventory entry; synthesise a minimal VM so the
		// manifest still carries a stable uuid + architecture. Real agents
		// would 404 an unknown VM, but the e2e seeds the VM CP-side, not via
		// the mock VmsCreate path, so the mock tolerates the absence here.
		vm = AgentVM{ID: uuid.New(), Name: vmName, Architecture: m.architecture}
	}
	manifest := mockSnapshotManifest(vm, body.Name, description, now)
	m.state.tasks[agentTaskID] = &agentTask{
		id:               agentTaskID,
		taskType:         "vm.snapshot.create",
		resourceType:     "snapshot",
		resourceID:       vm.ID,
		createdAt:        now,
		delay:            defaultPoolScanDelay,
		snapshotManifest: &manifest,
	}
	m.state.mu.Unlock()

	m.respondJSON(w, r, opID, http.StatusAccepted, agentapi.AsyncTaskAccepted{
		TaskID: agentTaskID,
		Status: agentapi.AsyncTaskAcceptedStatus("pending"),
		Links: struct {
			Self string `json:"self"`
		}{Self: "/v1/tasks/" + agentTaskID.String()},
	})
}

// VMSnapshotsList implements GET /v1/vms/{vm_name}/snapshots. Returns the
// materialised snapshots for the VM as a bare array of agentapi.Snapshot (the
// agent.yaml list shape). Empty array (not null) when none exist.
func (m *Mock) VMSnapshotsList(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName) {
	const opID = "vmSnapshots.list"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.state.mu.Lock()
	bucket := m.state.snapshots[vmName]
	out := make([]agentapi.Snapshot, 0, len(bucket))
	for _, s := range bucket {
		out = append(out, s.toAPI())
	}
	m.state.mu.Unlock()
	m.respondJSON(w, r, opID, http.StatusOK, out)
}

// VMSnapshotsGet implements GET /v1/vms/{vm_name}/snapshots/{snapshot_name}.
// Returns the materialised snapshot or 404.
func (m *Mock) VMSnapshotsGet(w http.ResponseWriter, r *http.Request, vmName agentapi.VMName, snapshotName agentapi.SnapshotName) {
	const opID = "vmSnapshots.get"
	if m.preDispatch(w, r, opID) {
		return
	}
	m.state.mu.Lock()
	s, ok := m.state.snapshots[vmName][snapshotName]
	m.state.mu.Unlock()
	if !ok {
		m.respondNotFound(w, r, opID, "snapshot not found on this agent")
		return
	}
	m.respondJSON(w, r, opID, http.StatusOK, s.toAPI())
}

// SnapshotsDelete implements DELETE /v1/snapshots/{vm_id}/{snapshot_name}. The
// route is node-level and keyed on the immutable vm_id (NOT the live VM), so the
// delete works after the source VM is gone - the mock reverse-resolves vm_id to a
// VM name only for its name-keyed introspection bookkeeping. Records the orphaned
// blob digests the CP asked the agent to GC (the fail-closed delete set, passed as
// repeated `orphaned` query params) so a test can assert the set propagated,
// removes the materialised entry, mints a snapshot-delete agent task that polls to
// success, and returns 202.
func (m *Mock) SnapshotsDelete(w http.ResponseWriter, r *http.Request, vmID uuid.UUID, snapshotName agentapi.SnapshotName, _ agentapi.SnapshotsDeleteParams) {
	const opID = "snapshots.delete"
	if m.preDispatch(w, r, opID) {
		return
	}

	orphaned := append([]string(nil), r.URL.Query()["orphaned"]...)
	agentTaskID := uuid.New()
	now := time.Now().UTC()

	m.state.mu.Lock()
	// Reverse-resolve vm_id -> vm name to drop the name-keyed materialised entry and
	// record the introspection ask. The VM may already be gone (the whole point of
	// the by-id route); a miss just leaves nothing to drop, keyed by vm_id.
	vmName := vmID.String()
	for name, vm := range m.state.storedVMs {
		if vm.ID == vmID {
			vmName = name
			break
		}
	}
	if bucket, ok := m.state.snapshots[vmName]; ok {
		delete(bucket, snapshotName)
	}
	m.state.snapshotDeleteAsks[snapshotKey{VMName: vmName, SnapshotName: snapshotName}] = orphaned
	m.state.tasks[agentTaskID] = &agentTask{
		id:                   agentTaskID,
		taskType:             "vm.snapshot.delete",
		resourceType:         "snapshot",
		resourceID:           vmID,
		createdAt:            now,
		delay:                defaultPoolScanDelay,
		snapshotDeleteResult: &struct{}{},
	}
	m.state.mu.Unlock()

	m.respondJSON(w, r, opID, http.StatusAccepted, agentapi.AsyncTaskAccepted{
		TaskID: agentTaskID,
		Status: agentapi.AsyncTaskAcceptedStatus("pending"),
		Links: struct {
			Self string `json:"self"`
		}{Self: "/v1/tasks/" + agentTaskID.String()},
	})
}

// projectSnapshotCreateTerminal fills the Task with the vm.snapshot.create
// terminal projection: the produced agentapi.Snapshot manifest under task.result
// on success. The CP worker's decodeSnapshotResult re-marshals this map and
// unmarshals into agentapi.Snapshot, reading disks[] + vm_state_at_snapshot.
func projectSnapshotCreateTerminal(task *agentapi.Task, t *agentTask) {
	if t.snapshotManifest == nil {
		task.Status = agentapi.TaskStatus("failed")
		return
	}
	task.Status = agentapi.TaskStatus("success")
	raw, err := json.Marshal(t.snapshotManifest.toAPI())
	if err != nil {
		task.Status = agentapi.TaskStatus("failed")
		return
	}
	var resultMap map[string]any
	if err := json.Unmarshal(raw, &resultMap); err != nil {
		task.Status = agentapi.TaskStatus("failed")
		return
	}
	task.Result = &resultMap
}

// projectSnapshotDeleteTerminal fills the Task with the vm.snapshot.delete
// terminal projection: empty result map on success (the agent contract surfaces
// nothing under result for a delete).
func projectSnapshotDeleteTerminal(task *agentapi.Task) {
	task.Status = agentapi.TaskStatus("success")
	resultMap := map[string]any{}
	task.Result = &resultMap
}

// materializeSnapshotCreateLocked stages the manifest into state.snapshots once
// the create task has reached terminal-success, so the list / get surface
// observes it. Caller holds m.state.mu. Operates on the live state, not on the
// snapshot copy passed to projectAgentTask.
func (m *Mock) materializeSnapshotCreateLocked(snapshot *agentTask, now time.Time) {
	if snapshot.taskType != "vm.snapshot.create" || snapshot.snapshotManifest == nil {
		return
	}
	if now.Sub(snapshot.createdAt) < snapshot.delay {
		return
	}
	man := *snapshot.snapshotManifest
	if m.state.snapshots[man.vmName] == nil {
		m.state.snapshots[man.vmName] = map[string]mockSnapshot{}
	}
	m.state.snapshots[man.vmName][man.name] = man
}

// SnapshotDeleteAsk returns the orphaned blob digests the CP asked the agent to
// GC for (vmName, snapshotName) through SnapshotsDelete. The second return is
// false when no delete was recorded. Test introspection only.
func (m *Mock) SnapshotDeleteAsk(vmName, snapshotName string) ([]string, bool) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	v, ok := m.state.snapshotDeleteAsks[snapshotKey{VMName: vmName, SnapshotName: snapshotName}]
	if !ok {
		return nil, false
	}
	return append([]string(nil), v...), true
}
