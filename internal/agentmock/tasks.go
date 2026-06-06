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
// `storage_pool.scan` and the VM lifecycle task types.
// projectAgentTask switches on `taskType` to select the appropriate
// terminal-result projection, so future task types can extend the
// struct without reshaping the helper.
//
// Lifecycle is one-shot registration: the initiator handler
// (StoragePoolsScan / vmCreate / ...) inserts the row and never
// mutates it. tasks.get callers compute the OpenAPI Task projection
// from (createdAt + delay + result) at read time — no per-task
// goroutine or state-machine timer is involved.
type agentTask struct {
	id           uuid.UUID
	taskType     string
	resourceType string
	resourceID   uuid.UUID
	createdAt    time.Time
	delay        time.Duration

	// Scan-flavoured outcome. Set when taskType ==
	// "storage_pool.scan", zero value otherwise.
	result PoolScanResult

	// Iteration 3 Phase A: vm.create / vm.delete outcomes. Set
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

	// vm.create terminal-result correlation fields the CP worker's
	// createResultFromTerminal decode expects: the resolved image
	// content digest (echoed from the request's expected_sha256, or a
	// deterministic synthetic hash in compute mode) plus deterministic
	// virtual / disk sizes. Set only for taskType == "vm.create".
	vmImageSHA256      string
	vmVirtualSizeBytes int64
	vmDiskSizeBytes    int64

	// L2 async lifecycle (vm.start / vm.stop / vm.poweroff /
	// vm.reboot). vmLifecycleResult holds the queued synthetic
	// outcome; vmLifecycleName carries the inventory key for the
	// materialise hook; vmLifecycleOp drives the per-op default
	// transition when VMLifecycleResult.NewStatus is empty.
	vmLifecycleResult *VMLifecycleResult
	vmLifecycleName   string
	vmLifecycleOp     string
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

// projectVMCreateTerminal fills the Task with the vm.create
// terminal projection — vm_id plus the resolved image content digest
// and the virtual / disk sizes under task.result on success,
// ErrorEnvelope on failure. The CP worker's createResultFromTerminal
// decodes image_sha256 / virtual_size_bytes / disk_size_bytes onto
// the VM row + disk-runtime. The materialisation side effect (staging
// the AgentVM blueprint into state.storedVMs) is the TasksGet-handler's
// responsibility, not this projection.
func projectVMCreateTerminal(task *agentapi.Task, t *agentTask) {
	if t.vmCreateResult == nil {
		task.Status = agentapi.TaskStatus("failed")
		return
	}
	task.Status = agentapi.TaskStatus(t.vmCreateResult.Status)
	switch t.vmCreateResult.Status {
	case "success":
		resultMap := map[string]any{
			"vm_id":              t.resourceID.String(),
			"image_sha256":       t.vmImageSHA256,
			"virtual_size_bytes": t.vmVirtualSizeBytes,
			"disk_size_bytes":    t.vmDiskSizeBytes,
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
