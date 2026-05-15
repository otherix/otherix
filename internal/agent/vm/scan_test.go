// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// waitForTaskTerminal polls the in-memory TaskStore until the task
// reaches a terminal status (success / failed) или the budget expires.
// The scan goroutine runs statfs synchronously — microseconds на а
// healthy filesystem — so а short budget is sufficient.
func waitForTaskTerminal(t *testing.T, m *Manager, id uuid.UUID, budget time.Duration) *AgentTask {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		task := m.Task(id)
		if task == nil {
			t.Fatalf("task %s disappeared from store", id)
		}
		if task.Status == TaskStatusSuccess || task.Status == TaskStatusFailed {
			return task
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach terminal status within %s", id, budget)
	return nil
}

func TestManager_ScanPool_UnknownPool(t *testing.T) {
	cfg, _, _ := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = m.ScanPool(t.Context(), "unknown-pool")
	if !errors.Is(err, ErrPoolUnknown) {
		t.Fatalf("ScanPool(unknown name) = %v, want ErrPoolUnknown", err)
	}
}

func TestManager_ScanPool_StatfsFailure(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	// AddPool already created poolRoot и ran а write probe там;
	// deleting it before the scan triggers а statfs ENOENT failure.
	// Portable across CI environments (root-running tests would
	// bypass а chmod 000 fixture; ENOENT cannot be bypassed).
	if err := os.RemoveAll(poolRoot); err != nil {
		t.Fatalf("remove pool root: %v", err)
	}

	task, err := m.ScanPool(t.Context(), poolName)
	if err != nil {
		t.Fatalf("ScanPool: %v", err)
	}

	terminal := waitForTaskTerminal(t, m, task.ID, 2*time.Second)
	if terminal.Status != TaskStatusFailed {
		t.Fatalf("terminal.Status = %q, want %q",
			terminal.Status, TaskStatusFailed)
	}
	if terminal.Error == nil {
		t.Fatalf("terminal.Error = nil, want populated envelope")
	}
	if terminal.Error.Code != "scan_failed" {
		t.Errorf("terminal.Error.Code = %q, want %q",
			terminal.Error.Code, "scan_failed")
	}
	if terminal.Error.Message == "" {
		t.Errorf("terminal.Error.Message is empty")
	}
}
