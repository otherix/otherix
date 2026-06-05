// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
)

// syntheticImageSHA derives a deterministic 64-char lowercase-hex
// digest from the image URL, used as the mock's compute-mode result
// when the request leaves expected_sha256 empty. Stable per URL so
// repeated creates against the same image surface the same digest.
func syntheticImageSHA(imageURL string) string {
	sum := sha256.Sum256([]byte("agentmock:image:" + imageURL))
	return hex.EncodeToString(sum[:])
}

// vmCreateRequestBody mirrors the Iteration 1 agent's hand-written
// createRequest (internal/agent/handlers/vms/create.go) — the
// simplified shape the CP worker sends through agentclient.
// Decoding into this struct rather than agentapi.VMSpec is
// intentional: the richer VMSpec is a future design target; the wire
// on the test bench today is the simplified set. Drift is documented
// in the mvp-direct-qemu-agent sketch.
type vmCreateRequestBody struct {
	UUID     string `json:"uuid"`
	Name     string `json:"name"`
	VCPUs    int    `json:"vcpus"`
	MemoryMB int    `json:"memory_mb"`
	Pool     string `json:"pool"`
	// Image-based create wire fields (agentclient.VMCreateRequest):
	// ImageURL is the HTTPS source the agent ensures into the pool
	// cache, ExpectedSHA256 the optional content pin, Format the
	// image format ("qcow2" default), DiskGiB the requested root-disk
	// size (0 = default to the image's virtual size).
	ImageURL       string `json:"image_url"`
	ExpectedSHA256 string `json:"expected_sha256,omitempty"`
	Format         string `json:"format,omitempty"`
	DiskGiB        int    `json:"disk_gib,omitempty"`
	// UserData (L3) carries the CP-resolved cloud-init blob. Mock
	// stores it on the AgentVM record so integration tests can
	// assert the resolver picked the right source and the agent wire
	// shape propagates the field.
	UserData string `json:"user_data,omitempty"`
}

// vmView mirrors the Iteration 1 agent's hand-written response shape.
// Defined locally so the mock's wire output is fully controlled by
// this package — see comment on vmCreateRequestBody. The wire field
// is `pool` (was `pool_id`) for consistency with the CP edge; the
// value remains a UUID string because the agent has no DB of pool
// names.
type vmView struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	VCPUs        int    `json:"vcpus"`
	MemoryMB     int    `json:"memory_mb"`
	Pool         string `json:"pool"`
	Architecture string `json:"architecture"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func vmToView(v AgentVM) vmView {
	return vmView{
		ID:           v.ID.String(),
		Name:         v.Name,
		VCPUs:        v.VCPUs,
		MemoryMB:     v.MemoryMB,
		Pool:         v.PoolName,
		Architecture: v.Architecture,
		Status:       v.Status,
		CreatedAt:    v.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    v.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// vmListResponse mirrors the agent's listResponse envelope.
type vmListResponse struct {
	Data []vmView       `json:"data"`
	Meta map[string]any `json:"meta"`
}

// vmCreate decodes the request body, mints a fresh agent-task uuid,
// registers an `agentTask` carrying the create-flavoured outcome and
// the request-derived AgentVM blueprint, and returns 202 +
// AsyncTaskAccepted. The terminal projection (and AgentVM
// materialisation) lands lazily on later GET /v1/tasks/{id} calls —
// see projectVMCreateTerminal and materializeVMCreateLocked.
//
// Outcome selection:
//
//  1. If the requested VM name is already in state.storedVMs, the
//     handler emits a terminal-success projection without consuming
//     the AddVMCreateResult queue (idempotent re-create — the agent
//     contract treats repeat creates as no-ops on the wire).
//  2. Otherwise the next queued VMCreateResult drives the outcome;
//     an empty queue yields a default success.
func (m *Mock) vmCreate(w http.ResponseWriter, r *http.Request, opID string) {
	var body vmCreateRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		m.respondJSON(w, r, opID, http.StatusBadRequest, errorEnvelope{
			Error: errorEnvelopeBody{
				Code:    "validation_failed",
				Message: "decode VmsCreate body: " + err.Error(),
			},
		})
		return
	}

	vmID, err := uuid.Parse(body.UUID)
	if err != nil || vmID == uuid.Nil {
		m.respondJSON(w, r, opID, http.StatusBadRequest, errorEnvelope{
			Error: errorEnvelopeBody{
				Code:    "validation_failed",
				Message: "uuid is missing or not a valid UUID",
			},
		})
		return
	}
	if body.Name == "" {
		m.respondJSON(w, r, opID, http.StatusBadRequest, errorEnvelope{
			Error: errorEnvelopeBody{
				Code:    "validation_failed",
				Message: "name is required",
			},
		})
		return
	}
	if body.Pool == "" {
		m.respondJSON(w, r, opID, http.StatusBadRequest, errorEnvelope{
			Error: errorEnvelopeBody{
				Code:    "validation_failed",
				Message: "pool is required",
			},
		})
		return
	}

	agentTaskID := uuid.New()

	m.state.mu.Lock()
	now := time.Now().UTC()

	var outcome VMCreateResult
	if _, exists := m.state.storedVMs[body.Name]; exists {
		outcome = VMCreateResult{Status: "success", Delay: defaultPoolScanDelay}
	} else {
		outcome = m.state.takeVMCreateResultLocked(body.Name)
	}

	blueprint := AgentVM{
		ID:           vmID,
		Name:         body.Name,
		VCPUs:        body.VCPUs,
		MemoryMB:     body.MemoryMB,
		PoolName:     body.Pool,
		Architecture: m.architecture,
		Status:       "running",
		UserData:     body.UserData,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if outcome.Architecture != "" {
		blueprint.Architecture = outcome.Architecture
	}
	if outcome.VMStatus != "" {
		blueprint.Status = outcome.VMStatus
	}

	// Compute the terminal vm.create result correlation fields the CP
	// worker decodes via createResultFromTerminal. ExpectedSHA256, when
	// pinned, is echoed verbatim (the mock never re-hashes the synthetic
	// bytes); compute-mode requests get a deterministic synthetic hash
	// derived from the image URL so the resulting VM row carries a valid
	// 64-char lowercase-hex digest. Sizes are deterministic non-zero
	// values keyed on DiskGiB (default 2 GiB).
	imageSHA := body.ExpectedSHA256
	if imageSHA == "" {
		imageSHA = syntheticImageSHA(body.ImageURL)
	}
	diskSize := int64(body.DiskGiB) << 30
	if diskSize <= 0 {
		diskSize = 2 << 30
	}

	m.state.tasks[agentTaskID] = &agentTask{
		id:                 agentTaskID,
		taskType:           "vm.create",
		resourceType:       "vm",
		resourceID:         vmID,
		createdAt:          now,
		delay:              outcome.Delay,
		vmCreateResult:     &outcome,
		vmBlueprint:        &blueprint,
		vmImageSHA256:      imageSHA,
		vmVirtualSizeBytes: diskSize,
		vmDiskSizeBytes:    diskSize,
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

// vmDelete mints a task uuid for the requested VM and registers a
// delete-flavoured agentTask. The terminal projection materialises
// lazily; on terminal-success materializeVMDeleteLocked removes the
// AgentVM entry from state.storedVMs.
//
// Idempotency: 202 is returned even when the VM is not registered —
// matches the agent contract that delete-of-missing is a no-op
// (callers translating 404 → ok already short-circuit before
// dispatch). Tests that need explicit 404 semantics should use
// InjectError("vms.delete", ...). The Path D URL key is the VM name;
// the projection still surfaces the VM's UUID through `resource_id`
// when a matching AgentVM exists in the inventory, falling back to
// uuid.Nil for delete-of-missing.
func (m *Mock) vmDelete(w http.ResponseWriter, r *http.Request, vmName string, opID string) {
	agentTaskID := uuid.New()

	m.state.mu.Lock()
	now := time.Now().UTC()
	outcome := m.state.takeVMDeleteResultLocked(vmName)
	var resourceID uuid.UUID
	if v, exists := m.state.storedVMs[vmName]; exists {
		resourceID = v.ID
	}
	m.state.tasks[agentTaskID] = &agentTask{
		id:             agentTaskID,
		taskType:       "vm.delete",
		resourceType:   "vm",
		resourceID:     resourceID,
		createdAt:      now,
		delay:          outcome.Delay,
		vmDeleteResult: &outcome,
		vmDeleteName:   vmName,
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

// vmGet projects a single AgentVM into the wire view, or 404s.
func (m *Mock) vmGet(w http.ResponseWriter, r *http.Request, vmName string, opID string) {
	m.state.mu.Lock()
	v, ok := m.state.storedVMs[vmName]
	m.state.mu.Unlock()
	if !ok {
		m.respondNotFound(w, r, opID, "vm not found on this agent")
		return
	}
	m.respondJSON(w, r, opID, http.StatusOK, vmToView(v))
}

// vmLifecycle implements the L1 sync surface (pause / resume / reset).
// Looks up the stored AgentVM by name, validates the current status
// matches `requireStatus`, then transitions to `newStatus` and returns
// the refreshed entry. For reset, callers pass `newStatus == requireStatus`
// because the QMP `system_reset` action preserves runtime identity.
func (m *Mock) vmLifecycle(w http.ResponseWriter, r *http.Request, opID, vmName, requireStatus, newStatus string) {
	m.state.mu.Lock()
	v, ok := m.state.storedVMs[vmName]
	if !ok {
		m.state.mu.Unlock()
		m.respondNotFound(w, r, opID, "vm not found on this agent")
		return
	}
	if v.Status != requireStatus {
		m.state.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(errorEnvelope{Error: errorEnvelopeBody{
			Code:    "conflict",
			Message: opID + " requires status=" + requireStatus + ", got " + v.Status,
		}})
		m.recordRequest(opID, r, http.StatusConflict)
		return
	}
	v.Status = newStatus
	v.UpdatedAt = time.Now().UTC()
	m.state.storedVMs[vmName] = v
	m.state.mu.Unlock()
	m.respondJSON(w, r, opID, http.StatusOK, vmToView(v))
}

// vmList snapshots the inventory and emits the {data, meta} envelope.
func (m *Mock) vmList(w http.ResponseWriter, r *http.Request, opID string) {
	m.state.mu.Lock()
	vms := make([]vmView, 0, len(m.state.storedVMs))
	for _, v := range m.state.storedVMs {
		vms = append(vms, vmToView(v))
	}
	m.state.mu.Unlock()
	m.respondJSON(w, r, opID, http.StatusOK, vmListResponse{
		Data: vms,
		Meta: map[string]any{"next_cursor": nil},
	})
}

// materializeVMCreateLocked stages the AgentVM blueprint into
// state.storedVMs once the create task has reached terminal-success.
// Caller holds m.state.mu. Operates on the live state, not on the
// snapshot copy passed to projectAgentTask.
func (m *Mock) materializeVMCreateLocked(snapshot *agentTask, now time.Time) {
	if snapshot.taskType != "vm.create" || snapshot.vmCreateResult == nil || snapshot.vmBlueprint == nil {
		return
	}
	if snapshot.vmCreateResult.Status != "success" {
		return
	}
	if now.Sub(snapshot.createdAt) < snapshot.delay {
		return
	}
	if _, present := m.state.storedVMs[snapshot.vmBlueprint.Name]; present {
		return
	}
	m.state.storedVMs[snapshot.vmBlueprint.Name] = *snapshot.vmBlueprint
}

// materializeVMDeleteLocked removes the AgentVM entry from
// state.storedVMs once the delete task has reached terminal-success.
// Caller holds m.state.mu.
func (m *Mock) materializeVMDeleteLocked(snapshot *agentTask, now time.Time) {
	if snapshot.taskType != "vm.delete" || snapshot.vmDeleteResult == nil {
		return
	}
	if snapshot.vmDeleteResult.Status != "success" {
		return
	}
	if now.Sub(snapshot.createdAt) < snapshot.delay {
		return
	}
	delete(m.state.storedVMs, snapshot.vmDeleteName)
}

// vmLifecycleAsync registers a new async lifecycle agentTask for the
// supplied op (start / stop / poweroff / reboot) and returns 202 +
// AsyncTaskAccepted. The materialise hook (status transition) lands
// lazily on later TasksGet calls. Mirrors vmCreate / vmDelete shape.
func (m *Mock) vmLifecycleAsync(w http.ResponseWriter, r *http.Request, opID, vmName, op string) {
	agentTaskID := uuid.New()

	m.state.mu.Lock()
	now := time.Now().UTC()
	outcome := m.state.takeVMLifecycleResultLocked(vmName, op)
	var resourceID uuid.UUID
	if v, exists := m.state.storedVMs[vmName]; exists {
		resourceID = v.ID
	}
	m.state.tasks[agentTaskID] = &agentTask{
		id:                agentTaskID,
		taskType:          "vm." + op,
		resourceType:      "vm",
		resourceID:        resourceID,
		createdAt:         now,
		delay:             outcome.Delay,
		vmLifecycleResult: &outcome,
		vmLifecycleName:   vmName,
		vmLifecycleOp:     op,
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

// materializeVMLifecycleLocked transitions storedVMs[name].Status
// per the queued outcome once an async lifecycle task has reached
// terminal-success. Caller holds m.state.mu. Failed terminals are a
// no-op (the inventory entry stays in its prior phase per Area 4-IV
// — manual intervention only).
func (m *Mock) materializeVMLifecycleLocked(snapshot *agentTask, now time.Time) {
	if snapshot.vmLifecycleResult == nil || snapshot.vmLifecycleName == "" {
		return
	}
	switch snapshot.taskType {
	case "vm.start", "vm.stop", "vm.poweroff", "vm.reboot":
	default:
		return
	}
	if snapshot.vmLifecycleResult.Status != "success" {
		return
	}
	if now.Sub(snapshot.createdAt) < snapshot.delay {
		return
	}
	v, ok := m.state.storedVMs[snapshot.vmLifecycleName]
	if !ok {
		return
	}
	target := snapshot.vmLifecycleResult.NewStatus
	if target == "" {
		target = vmLifecycleOpDefaultTransition(snapshot.vmLifecycleOp)
	}
	v.Status = target
	v.UpdatedAt = now
	m.state.storedVMs[snapshot.vmLifecycleName] = v
}
