// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
)

// defaultPoolScanDelay is the time-to-terminal applied when a queued
// PoolScanResult leaves Delay zero. 100ms is long enough for a
// polling client to observe at least one `running` poll under
// realistic timing, short enough for tests to move quickly.
const defaultPoolScanDelay = 100 * time.Millisecond

// agentTask is the in-memory record of an agent-side executional
// task projected through GET /v1/tasks/{id}. Covers
// `storage_pool.scan` and `storage_image.import`.
// projectAgentTask switches on `taskType` to select the appropriate
// terminal-result projection, so future task types (vm.start,
// template.import, etc.) can extend the struct without reshaping
// the helper.
//
// Lifecycle is one-shot registration: the initiator handler
// (StoragePoolsScan / StorageImagesImport) inserts the row and
// never mutates it. tasks.get callers compute the OpenAPI Task
// projection from (createdAt + delay + result) at read time — no
// per-task goroutine or state-machine timer is involved.
type agentTask struct {
	id           uuid.UUID
	taskType     string
	resourceType string
	resourceID   uuid.UUID
	// poolName captures the agent-side pool key when the task targets
	// a pool resource (storage_pool.scan / storage_image.import). The
	// agent's pool registry is name-keyed; the materialise-on-tasks.get
	// side effect uses this to stage the image bucket under the right
	// name. Empty for VM tasks.
	poolName  string
	createdAt time.Time
	delay     time.Duration

	// Scan-flavoured outcome. Set when taskType ==
	// "storage_pool.scan", zero value otherwise.
	result PoolScanResult

	// Import-flavoured outcome. Set when taskType ==
	// "storage_image.import", nil otherwise. importChecksum mirrors
	// the request's expected_checksum_sha256 so the terminal-success
	// projection can surface it under task.result.
	importResult   *ImageImportResult
	importChecksum string

	// MVP Iteration 3 Phase A: vm.create / vm.delete outcomes. Set
	// when taskType == "vm.create" / "vm.delete", nil otherwise.
	// vmBlueprint carries the AgentVM that materialises into
	// state.storedVMs on terminal-success of a create task — the
	// blueprint is captured at handler time so the create result is
	// frozen against later state mutations. Per Pre-L1 Path D rekey
	// the delete-side key is the VM name (was UUID); the resourceID
	// field already carries the VM UUID for the wire projection.
	vmCreateResult *VMCreateResult
	vmBlueprint    *AgentVM
	vmDeleteResult *VMDeleteResult
	vmDeleteName   string

	// L2 async lifecycle (vm.start / vm.stop / vm.poweroff /
	// vm.reboot). vmLifecycleResult holds the queued synthetic
	// outcome; vmLifecycleName carries the inventory key for the
	// materialise hook; vmLifecycleOp drives the per-op default
	// transition when VMLifecycleResult.NewStatus is empty.
	vmLifecycleResult *VMLifecycleResult
	vmLifecycleName   string
	vmLifecycleOp     string
}

// defaultImageImportSize is the SizeBytes the mock projects when a
// queued ImageImportResult leaves SizeBytes zero — a 1 MiB qcow2
// stub, big enough for a non-zero assertion and small enough to
// stay clearly synthetic.
const defaultImageImportSize int64 = 1 << 20

// AddImageImportResult queues a synthetic outcome for the next
// StorageImagesImport call against (poolID, checksumSHA256). Calls
// are FIFO per (pool, checksum); an empty queue and absent staged
// image yield a default success ({SizeBytes: 1 MiB, Format:
// "qcow2", Delay: defaultPoolScanDelay}).
//
// Status defaults to "success" when empty; SizeBytes / Format /
// Delay default as documented above; Status values other than
// "success" / "failed" are stored verbatim.
//
// Pre-existence rule: when the import handler observes that the
// requested checksum is already staged in the pool (via AddImage or
// a prior successful import), the queue is NOT consumed —
// idempotent re-imports return terminal-success without touching
// the injected outcome.
func (m *Mock) AddImageImportResult(poolName, checksumSHA256 string, r ImageImportResult) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if m.state.imageImportResults == nil {
		m.state.imageImportResults = map[imageImportKey][]ImageImportResult{}
	}
	key := imageImportKey{PoolName: poolName, ChecksumSHA256: checksumSHA256}
	m.state.imageImportResults[key] = append(m.state.imageImportResults[key], r)
}

// takeImageImportResultLocked pops the next queued ImageImportResult
// for (poolName, checksumSHA256), normalising defaults. Empty queue
// yields a default success result. Caller holds mu.
func (s *state) takeImageImportResultLocked(poolName, checksumSHA256 string) ImageImportResult {
	key := imageImportKey{PoolName: poolName, ChecksumSHA256: checksumSHA256}
	queue := s.imageImportResults[key]
	if len(queue) == 0 {
		return ImageImportResult{
			Status:    "success",
			SizeBytes: defaultImageImportSize,
			Format:    "qcow2",
			Delay:     defaultPoolScanDelay,
		}
	}
	head := queue[0]
	if len(queue) == 1 {
		delete(s.imageImportResults, key)
	} else {
		s.imageImportResults[key] = queue[1:]
	}
	if head.Status == "" {
		head.Status = "success"
	}
	if head.SizeBytes <= 0 {
		head.SizeBytes = defaultImageImportSize
	}
	if head.Format == "" {
		head.Format = "qcow2"
	}
	if head.Delay <= 0 {
		head.Delay = defaultPoolScanDelay
	}
	return head
}

// AddPoolScanResult queues a synthetic outcome for the next
// StoragePoolsScan invocation against poolID. Calls are FIFO per
// pool; an empty queue yields a default success with zero capacity
// / available bytes and the defaultPoolScanDelay time-to-terminal.
//
// Status defaults to "success" when empty; Delay defaults to
// defaultPoolScanDelay when zero. Status values other than
// "success" and "failed" are stored verbatim — the projection
// surfaces them in the OpenAPI Task.status field, which is useful
// for tests that exercise non-canonical status handling on the CP
// side.
func (m *Mock) AddPoolScanResult(poolName string, r PoolScanResult) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if m.state.poolScanResults == nil {
		m.state.poolScanResults = map[string][]PoolScanResult{}
	}
	m.state.poolScanResults[poolName] = append(m.state.poolScanResults[poolName], r)
}

// takePoolScanResultLocked pops the next queued PoolScanResult for
// poolName, normalising defaults. An empty queue yields a default
// success result. Caller holds mu.
func (s *state) takePoolScanResultLocked(poolName string) PoolScanResult {
	queue := s.poolScanResults[poolName]
	if len(queue) == 0 {
		return PoolScanResult{Status: "success", Delay: defaultPoolScanDelay}
	}
	head := queue[0]
	if len(queue) == 1 {
		delete(s.poolScanResults, poolName)
	} else {
		s.poolScanResults[poolName] = queue[1:]
	}
	if head.Status == "" {
		head.Status = "success"
	}
	if head.Delay <= 0 {
		head.Delay = defaultPoolScanDelay
	}
	return head
}

// projectAgentTask renders the OpenAPI Task projection of t at the
// given instant. Three regions, by elapsed time since creation:
//
//   - elapsed < delay/2:        status=pending, no started_at.
//   - delay/2 ≤ elapsed < delay: status=running, started_at = createdAt + delay/2.
//   - elapsed ≥ delay:           terminal status from result.Status,
//     started_at and finished_at populated, result map or error
//     envelope wired.
//
// Pure function: no state mutation, safe to call without the lock.
// Callers must therefore copy the *agentTask under the lock before
// invoking this helper.
func projectAgentTask(t *agentTask, now time.Time) agentapi.Task {
	resourceID := t.resourceID.String()
	task := agentapi.Task{
		ID:           t.id,
		Type:         t.taskType,
		ResourceType: t.resourceType,
		ResourceID:   &resourceID,
		Attempts:     1,
		MaxAttempts:  25,
		CreatedAt:    t.createdAt,
	}

	elapsed := now.Sub(t.createdAt)
	half := t.delay / 2

	if elapsed < half {
		task.Status = agentapi.TaskStatus("pending")
		return task
	}

	startedAt := t.createdAt.Add(half)
	task.StartedAt = &startedAt

	if elapsed < t.delay {
		task.Status = agentapi.TaskStatus("running")
		return task
	}

	finishedAt := t.createdAt.Add(t.delay)
	task.FinishedAt = &finishedAt

	switch t.taskType {
	case "storage_image.import":
		projectImportTerminal(&task, t, finishedAt)
	case "vm.create":
		projectVMCreateTerminal(&task, t)
	case "vm.delete":
		projectVMDeleteTerminal(&task, t)
	case "vm.start", "vm.stop", "vm.poweroff", "vm.reboot":
		projectVMLifecycleTerminal(&task, t)
	default:
		// "storage_pool.scan" and any future scan-shaped task type
		// fall through to the scan projection.
		projectScanTerminal(&task, t, finishedAt)
	}
	return task
}

// projectScanTerminal fills the Task with the storage_pool.scan
// terminal projection — capacity / availability / reported_at on
// success, ErrorEnvelope on failure.
func projectScanTerminal(task *agentapi.Task, t *agentTask, finishedAt time.Time) {
	task.Status = agentapi.TaskStatus(t.result.Status)
	switch t.result.Status {
	case "success":
		resultMap := map[string]any{
			"capacity_bytes":  t.result.CapacityBytes,
			"available_bytes": t.result.AvailableBytes,
			"reported_at":     finishedAt.Format(time.RFC3339Nano),
		}
		task.Result = &resultMap
	case "failed", "cancelled":
		if t.result.Error != nil {
			task.Error = projectErrorEnvelope(t.result.Error)
		}
	}
}

// projectImportTerminal fills the Task with the storage_image.import
// terminal projection — checksum / size / format on success,
// ErrorEnvelope on failure. The checksum surfaced under
// task.result is the request's expected_checksum_sha256, since the
// mock never re-hashes the (synthetic) bytes.
func projectImportTerminal(task *agentapi.Task, t *agentTask, _ time.Time) {
	if t.importResult == nil {
		// Defensive: a storage_image.import task without an attached
		// outcome is a programming error; surface it as a failed
		// terminal so tests don't observe a stuck `running`.
		task.Status = agentapi.TaskStatus("failed")
		return
	}
	task.Status = agentapi.TaskStatus(t.importResult.Status)
	switch t.importResult.Status {
	case "success":
		resultMap := map[string]any{
			"checksum_sha256": t.importChecksum,
			"size_bytes":      t.importResult.SizeBytes,
			"format":          t.importResult.Format,
		}
		task.Result = &resultMap
	case "failed", "cancelled":
		if t.importResult.Error != nil {
			task.Error = projectErrorEnvelope(t.importResult.Error)
		}
	}
}

// projectVMCreateTerminal fills the Task with the vm.create
// terminal projection — vm_id under task.result on success,
// ErrorEnvelope on failure. The materialisation side effect (staging
// the AgentVM blueprint into state.storedVMs) is the
// TasksGet-handler's responsibility, not this projection.
func projectVMCreateTerminal(task *agentapi.Task, t *agentTask) {
	if t.vmCreateResult == nil {
		task.Status = agentapi.TaskStatus("failed")
		return
	}
	task.Status = agentapi.TaskStatus(t.vmCreateResult.Status)
	switch t.vmCreateResult.Status {
	case "success":
		resultMap := map[string]any{
			"vm_id": t.resourceID.String(),
		}
		task.Result = &resultMap
	case "failed", "cancelled":
		if t.vmCreateResult.Error != nil {
			task.Error = projectErrorEnvelope(t.vmCreateResult.Error)
		}
	}
}

// projectVMDeleteTerminal fills the Task with the vm.delete terminal
// projection — empty result map on success (the agent contract
// surfaces the deleted vm_id through resource_id, not result),
// ErrorEnvelope on failure.
func projectVMDeleteTerminal(task *agentapi.Task, t *agentTask) {
	if t.vmDeleteResult == nil {
		task.Status = agentapi.TaskStatus("failed")
		return
	}
	task.Status = agentapi.TaskStatus(t.vmDeleteResult.Status)
	switch t.vmDeleteResult.Status {
	case "success":
		resultMap := map[string]any{
			"vm_id": t.resourceID.String(),
		}
		task.Result = &resultMap
	case "failed", "cancelled":
		if t.vmDeleteResult.Error != nil {
			task.Error = projectErrorEnvelope(t.vmDeleteResult.Error)
		}
	}
}

// projectVMLifecycleTerminal fills the Task with the async lifecycle
// (start / stop / poweroff / reboot) terminal projection. Same shape
// as create / delete success — `vm_id` under task.result. The
// materialisation side effect (transitioning storedVMs[name].Status)
// lives in materializeVMLifecycleLocked, invoked from the TasksGet
// handler.
func projectVMLifecycleTerminal(task *agentapi.Task, t *agentTask) {
	if t.vmLifecycleResult == nil {
		task.Status = agentapi.TaskStatus("failed")
		return
	}
	task.Status = agentapi.TaskStatus(t.vmLifecycleResult.Status)
	switch t.vmLifecycleResult.Status {
	case "success":
		resultMap := map[string]any{
			"vm_id": t.resourceID.String(),
		}
		task.Result = &resultMap
	case "failed", "cancelled":
		if t.vmLifecycleResult.Error != nil {
			task.Error = projectErrorEnvelope(t.vmLifecycleResult.Error)
		}
	}
}

// vmLifecycleOpDefaultTransition returns the inventory phase the
// materialise hook applies on terminal-success when the queued
// VMLifecycleResult.NewStatus is empty. Mirrors the per-op semantics
// the real agent honours: start → running, stop / poweroff → stopped,
// reboot → running (PID changes but wire status converges back to
// running). Unknown op falls back to "running" — defensive only;
// production code paths only ever pass the four enumerated ops.
func vmLifecycleOpDefaultTransition(op string) string {
	switch op {
	case "start":
		return "running"
	case "stop":
		return "stopped"
	case "poweroff":
		return "stopped"
	case "reboot":
		return "running"
	default:
		return "running"
	}
}

// takeVMLifecycleResultLocked pops the next queued VMLifecycleResult
// for (vmName, op), normalising defaults. An empty queue yields a
// default success. Caller holds mu.
func (s *state) takeVMLifecycleResultLocked(vmName, op string) VMLifecycleResult {
	key := vmLifecycleKey{VMName: vmName, Op: op}
	queue := s.vmLifecycleResults[key]
	if len(queue) == 0 {
		return VMLifecycleResult{Status: "success", Delay: defaultPoolScanDelay}
	}
	head := queue[0]
	if len(queue) == 1 {
		delete(s.vmLifecycleResults, key)
	} else {
		s.vmLifecycleResults[key] = queue[1:]
	}
	if head.Status == "" {
		head.Status = "success"
	}
	if head.Delay <= 0 {
		head.Delay = defaultPoolScanDelay
	}
	return head
}

// takeVMCreateResultLocked pops the next queued VMCreateResult for
// vmName, normalising defaults. An empty queue yields a default success.
// Caller holds mu.
func (s *state) takeVMCreateResultLocked(vmName string) VMCreateResult {
	queue := s.vmCreateResults[vmName]
	if len(queue) == 0 {
		return VMCreateResult{Status: "success", Delay: defaultPoolScanDelay}
	}
	head := queue[0]
	if len(queue) == 1 {
		delete(s.vmCreateResults, vmName)
	} else {
		s.vmCreateResults[vmName] = queue[1:]
	}
	if head.Status == "" {
		head.Status = "success"
	}
	if head.Delay <= 0 {
		head.Delay = defaultPoolScanDelay
	}
	return head
}

// takeVMDeleteResultLocked pops the next queued VMDeleteResult for
// vmName, normalising defaults. An empty queue yields a default success.
// Caller holds mu.
func (s *state) takeVMDeleteResultLocked(vmName string) VMDeleteResult {
	queue := s.vmDeleteResults[vmName]
	if len(queue) == 0 {
		return VMDeleteResult{Status: "success", Delay: defaultPoolScanDelay}
	}
	head := queue[0]
	if len(queue) == 1 {
		delete(s.vmDeleteResults, vmName)
	} else {
		s.vmDeleteResults[vmName] = queue[1:]
	}
	if head.Status == "" {
		head.Status = "success"
	}
	if head.Delay <= 0 {
		head.Delay = defaultPoolScanDelay
	}
	return head
}

// projectErrorEnvelope adapts an agentmock ErrorEnvelope into the
// codegen Task.Error pointer-of-anonymous-struct shape. Returns nil
// for a nil envelope so the JSON-encoder omits the field.
func projectErrorEnvelope(env *ErrorEnvelope) *struct {
	Code    *agentapi.ErrorCode `json:"code,omitempty"`
	Details *map[string]any     `json:"details,omitempty"`
	Message *string             `json:"message,omitempty"`
} {
	out := &struct {
		Code    *agentapi.ErrorCode `json:"code,omitempty"`
		Details *map[string]any     `json:"details,omitempty"`
		Message *string             `json:"message,omitempty"`
	}{}
	if env.Code != "" {
		code := agentapi.ErrorCode(env.Code)
		out.Code = &code
	}
	if env.Message != "" {
		msg := env.Message
		out.Message = &msg
	}
	if len(env.Details) > 0 {
		details := env.Details
		out.Details = &details
	}
	return out
}
