// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
)

// TaskStatus enumerates the lifecycle of an agent task.
type TaskStatus string

// TaskStatus values surfaced through GET /v1/tasks/{id}.
const (
	TaskStatusPending TaskStatus = "pending"
	TaskStatusRunning TaskStatus = "running"
	TaskStatusSuccess TaskStatus = "success"
	TaskStatusFailed  TaskStatus = "failed"
)

// TaskKind identifies what a task is doing. Iteration 1 has two kinds;
// future iterations layer on additional kinds (vm.start, vm.stop,
// vm.migrate) without changing the surface.
type TaskKind string

// TaskKind values minted by the VM manager.
const (
	TaskKindVMCreate   TaskKind = "vm.create"
	TaskKindVMDelete   TaskKind = "vm.delete"
	TaskKindVMStart    TaskKind = "vm.start"
	TaskKindVMStop     TaskKind = "vm.stop"
	TaskKindVMPoweroff TaskKind = "vm.poweroff"
	TaskKindVMReboot   TaskKind = "vm.reboot"
	// TaskKindStoragePoolScan tracks a POST /v1/storage-pools/{id}/scan
	// task. The TaskStore.Create call carries the pool's UUID through
	// the `vmID` parameter — transitional reuse pending the broader
	// taskView contract alignment (rename `VMID` → `ResourceID`, json
	// `vm_id` → `resource_id`) tracked under contract-test parity в
	// ROADMAP.
	TaskKindStoragePoolScan TaskKind = "storage_pool.scan"
	// TaskKindStorageImageImport tracks a POST
	// /v1/storage-pools/{pool_id}/images task. Same transitional
	// `vmID` parameter reuse as TaskKindStoragePoolScan — the pool's
	// UUID rides through. The terminal-success Result carries
	// `{checksum_sha256, size_bytes, format}` matching the CP-side
	// agent_import_executor.decodeImportResult contract.
	TaskKindStorageImageImport TaskKind = "storage_image.import"
)

// TaskError mirrors the agent error envelope when a task fails. Code
// is a snake_case identifier; Details is optional context.
type TaskError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// AgentTask is an agent-local task record. Lost on agent restart; the
// CP can reissue if it cares. The Result field carries arbitrary
// success payloads so future task kinds can return their own shapes;
// vm.create currently returns {"vm_id": "<uuid>"}.
type AgentTask struct {
	ID        uuid.UUID
	Kind      TaskKind
	Status    TaskStatus
	VMID      uuid.UUID
	Error     *TaskError
	Result    json.RawMessage
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TaskStore is a goroutine-safe in-memory task registry.
type TaskStore struct {
	mu    sync.Mutex
	tasks map[uuid.UUID]*AgentTask
}

// NewTaskStore returns an empty TaskStore.
func NewTaskStore() *TaskStore {
	return &TaskStore{tasks: map[uuid.UUID]*AgentTask{}}
}

// Create allocates a new task ID and inserts the task in pending
// status. Returns а **copy** of the task: the live pointer stays
// inside the store and is mutated by Update under the store lock; the
// caller's copy is а stable initial snapshot they can hand к the HTTP
// layer (status="pending") без racing the goroutine that immediately
// transitions to "running" inside Update. Callers reuse only the ID
// for the goroutine handle.
func (s *TaskStore) Create(kind TaskKind, vmID uuid.UUID) *AgentTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	t := &AgentTask{
		ID:        uuid.New(),
		Kind:      kind,
		Status:    TaskStatusPending,
		VMID:      vmID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.tasks[t.ID] = t
	cp := *t
	return &cp
}

// Get returns a copy of the task, or nil when no such task exists.
// Returning a copy keeps callers from observing concurrent updates
// while holding no lock.
func (s *TaskStore) Get(id uuid.UUID) *AgentTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil
	}
	cp := *t
	return &cp
}

// Update mutates the task identified by id with apply, holding the
// store lock for the duration of apply. Returns false when no task
// matches id.
func (s *TaskStore) Update(id uuid.UUID, apply func(*AgentTask)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return false
	}
	apply(t)
	t.UpdatedAt = time.Now().UTC()
	return true
}
