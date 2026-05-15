// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ScanPool reports the current filesystem state of the pool identified
// by poolName. The task is created in `pending` status и the caller
// receives 202 + AsyncTaskAccepted immediately; statfs runs в а
// goroutine и the terminal status (success / failed) surfaces via
// GET /v1/tasks/{task_id}.
//
// The goroutine pattern is forced by api/openapi/agent.yaml — the
// AsyncTaskAccepted.status enum is strictly [pending, running], so а
// synchronous statfs cannot return its terminal result в the 202
// body. Goroutine isolation also makes the path extensible к the
// longer scans (image-cache enumeration) future iterations will need.
//
// Returns (nil, ErrPoolUnknown) when poolName is not в the agent's
// configured pool registry. The HTTP layer maps that к 404 not_found.
func (m *Manager) ScanPool(ctx context.Context, poolName string) (*AgentTask, error) {
	m.poolsMu.RLock()
	p, ok := m.pools[poolName]
	m.poolsMu.RUnlock()
	if !ok {
		return nil, ErrPoolUnknown
	}

	// TaskStore.Create's `vmID` parameter is overloaded к carry the
	// scan task's correlation id. The pool registry is name-keyed so
	// the field carries а freshly-minted uuid за per-task correlation
	// only. See TaskKindStoragePoolScan doc-comment в tasks.go.
	task := m.tasks.Create(TaskKindStoragePoolScan, uuid.New())

	// #nosec G118 -- async task work intentionally outlives the HTTP
	// request; the task surface (GET /v1/tasks/{id}) is how the CP
	// tracks progress.
	go m.runScan(task.ID, p.root)
	return task, nil
}

// runScan executes the filesystem stat against poolRoot и transitions
// the task к its terminal state. statfs is microseconds on healthy
// filesystems; the goroutine isolation exists for AsyncTaskAccepted
// contract conformance (see ScanPool docs), not because the work itself
// is long.
func (m *Manager) runScan(taskID uuid.UUID, poolRoot string) {
	log := m.log.With("task_id", taskID.String(), "pool_root", poolRoot)
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	total, available, err := pathFilesystemStats(poolRoot)
	if err != nil {
		log.Error("statfs pool root", "err", err)
		m.tasks.Update(taskID, func(t *AgentTask) {
			t.Status = TaskStatusFailed
			t.Error = &TaskError{
				Code:    "scan_failed",
				Message: err.Error(),
			}
		})
		return
	}
	if total == nil || available == nil {
		// Defensive — the linux impl always returns non-nil on err==nil,
		// but the non-Linux stub returns an explicit error; keep both
		// branches honest.
		m.tasks.Update(taskID, func(t *AgentTask) {
			t.Status = TaskStatusFailed
			t.Error = &TaskError{
				Code:    "scan_failed",
				Message: "statfs returned nil metrics",
			}
		})
		return
	}

	result, mErr := json.Marshal(map[string]any{
		"capacity_bytes":  *total,
		"available_bytes": *available,
		"reported_at":     time.Now().UTC().Format(time.RFC3339Nano),
	})
	if mErr != nil {
		log.Error("marshal scan result", "err", mErr)
		m.tasks.Update(taskID, func(t *AgentTask) {
			t.Status = TaskStatusFailed
			t.Error = &TaskError{
				Code:    "scan_failed",
				Message: fmt.Sprintf("marshal result: %v", mErr),
			}
		})
		return
	}

	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusSuccess
		t.Result = result
	})
	log.Info("scan completed",
		"capacity_bytes", *total,
		"available_bytes", *available,
	)
}
