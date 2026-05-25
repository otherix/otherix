// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package vm

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestManager_ScanPool_HappyPath exercises ScanPool end-to-end against
// a real Linux statfs. Linux-only because the !linux build tag of
// pathFilesystemStats returns an explicit error — there is no portable
// filesystem-stats syscall the agent could fall back to. Skipped on
// macOS dev hosts via //go:build linux; runs in CI ubuntu-24.04 and on
// any Linux host.
func TestManager_ScanPool_HappyPath(t *testing.T) {
	cfg, poolRoot, poolName := newTestConfig(t)
	m, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Manager.New starts with empty pool registry; the reconciler
	// populates it in production. Test seeds the registry directly to
	// mirror what a post-heartbeat state would look like.
	if err := m.AddPool(poolName, poolRoot); err != nil {
		t.Fatalf("AddPool: %v", err)
	}

	task, err := m.ScanPool(t.Context(), poolName)
	if err != nil {
		t.Fatalf("ScanPool: %v", err)
	}
	if task == nil || task.ID == uuid.Nil {
		t.Fatalf("ScanPool returned nil or zero task")
	}
	if task.Kind != TaskKindStoragePoolScan {
		t.Errorf("task.Kind = %q, want %q", task.Kind, TaskKindStoragePoolScan)
	}

	terminal := waitForTaskTerminal(t, m, task.ID, 2*time.Second)
	if terminal.Status != TaskStatusSuccess {
		t.Fatalf("terminal.Status = %q, want %q; error = %+v",
			terminal.Status, TaskStatusSuccess, terminal.Error)
	}

	var result struct {
		CapacityBytes  int64  `json:"capacity_bytes"`
		AvailableBytes int64  `json:"available_bytes"`
		ReportedAt     string `json:"reported_at"`
	}
	if err := json.Unmarshal(terminal.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.CapacityBytes <= 0 {
		t.Errorf("capacity_bytes = %d, want > 0", result.CapacityBytes)
	}
	if result.AvailableBytes < 0 || result.AvailableBytes > result.CapacityBytes {
		t.Errorf("available_bytes = %d, want in [0, %d]",
			result.AvailableBytes, result.CapacityBytes)
	}
	if _, err := time.Parse(time.RFC3339Nano, result.ReportedAt); err != nil {
		t.Errorf("reported_at = %q does not parse as RFC3339Nano: %v",
			result.ReportedAt, err)
	}
}
