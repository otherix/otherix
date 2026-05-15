// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestTaskStore_CreateGet(t *testing.T) {
	s := NewTaskStore()
	vmID := uuid.New()

	task := s.Create(TaskKindVMCreate, vmID)
	if task.Status != TaskStatusPending {
		t.Errorf("initial status = %s, want %s", task.Status, TaskStatusPending)
	}
	if task.VMID != vmID {
		t.Errorf("vm id = %s, want %s", task.VMID, vmID)
	}

	got := s.Get(task.ID)
	if got == nil || got.ID != task.ID {
		t.Errorf("Get(%s) = %+v, want task", task.ID, got)
	}

	if s.Get(uuid.New()) != nil {
		t.Errorf("Get(missing) = non-nil")
	}
}

func TestTaskStore_Update(t *testing.T) {
	s := NewTaskStore()
	task := s.Create(TaskKindVMCreate, uuid.New())

	if !s.Update(task.ID, func(t *AgentTask) { t.Status = TaskStatusRunning }) {
		t.Fatalf("Update existing = false, want true")
	}
	got := s.Get(task.ID)
	if got.Status != TaskStatusRunning {
		t.Errorf("status after update = %s, want %s", got.Status, TaskStatusRunning)
	}
	if !got.UpdatedAt.After(task.CreatedAt) && !got.UpdatedAt.Equal(task.CreatedAt) {
		t.Errorf("UpdatedAt should advance after Update")
	}

	if s.Update(uuid.New(), func(*AgentTask) {}) {
		t.Errorf("Update missing = true, want false")
	}
}

func TestTaskStore_GetReturnsCopy(t *testing.T) {
	s := NewTaskStore()
	task := s.Create(TaskKindVMCreate, uuid.New())

	got := s.Get(task.ID)
	got.Status = TaskStatusFailed // mutate the returned copy

	again := s.Get(task.ID)
	if again.Status == TaskStatusFailed {
		t.Errorf("mutating Get result leaked into store: %+v", again)
	}
}

func TestTaskStore_ConcurrentAccess(t *testing.T) {
	s := NewTaskStore()
	tasks := make([]*AgentTask, 50)
	for i := range tasks {
		tasks[i] = s.Create(TaskKindVMCreate, uuid.New())
	}

	var wg sync.WaitGroup
	for _, task := range tasks {
		wg.Add(2)
		go func(id uuid.UUID) {
			defer wg.Done()
			s.Update(id, func(t *AgentTask) { t.Status = TaskStatusRunning })
		}(task.ID)
		go func(id uuid.UUID) {
			defer wg.Done()
			_ = s.Get(id)
		}(task.ID)
	}
	wg.Wait()

	for _, task := range tasks {
		got := s.Get(task.ID)
		if got.Status != TaskStatusRunning {
			t.Errorf("task %s status = %s, want running", task.ID, got.Status)
		}
	}
}
