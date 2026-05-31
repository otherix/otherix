// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/store"
)

// This file holds the queue contract for the
// storage_image.import task: the job-arg payload, the executor seam + its
// argument/result shapes, the terminal error-code catalogue, the error
// classifier, and the result marshaller. The worker implementation that
// consumes this contract lives in run.go.

// StorageImageImportArgs is the queue job-args payload for a
// `storage_image.import` task. The atomic-enqueue handler inserts the row,
// mints a fresh task id, and enqueues this payload; the worker resolves
// template / pool / node before dispatching to the executor.
type StorageImageImportArgs struct {
	TaskID     uuid.UUID `json:"task_id"`
	TemplateID uuid.UUID `json:"template_id"`
	PoolID     uuid.UUID `json:"pool_id"`
}

// Kind names the job kind. Mirrors the OpenAPI Task.type value surfaced through
// tasks.{list,get}.
func (StorageImageImportArgs) Kind() string { return "storage_image.import" }

// ImportArgs is the per-task input the executor receives. Distinct from
// StorageImageImportArgs (the queue job args): the worker resolves template,
// pool, and node before dispatching, so the executor sees a self-contained
// dependency-free struct.
//
// AgentTaskID carries the persisted agent-side task uuid for resumption - nil
// on first run, non-nil when the worker is recovering from a CP-side restart.
//
// OnAgentTaskID is the callback the executor invokes immediately after the
// agent's 202 returns, persisting the agent task id. The callback shape keeps
// the executor seam narrow - the executor never touches the store directly.
type ImportArgs struct {
	TaskID        uuid.UUID
	AgentTaskID   *uuid.UUID
	Pool          store.StoragePool
	Node          store.Node
	Template      store.Template
	OnAgentTaskID func(ctx context.Context, agentTaskID uuid.UUID) error
}

// ImportResult is the executor's output. Surfaces verbatim into storage_images
// and into the task's `result` JSONB column.
type ImportResult struct {
	ChecksumSHA256 string `json:"checksum_sha256"`
	SizeBytes      int64  `json:"size_bytes"`
	Format         string `json:"format"`
}

// ImportExecutor is the per-task-type seam. The production implementation
// (agentImportExecutor) lives in this package; tests pass an in-test fake.
type ImportExecutor interface {
	Execute(ctx context.Context, args ImportArgs) (ImportResult, error)
}

// Failure code constants for the task `error` envelope. Passthrough codes
// (qcow2_header_invalid, checksum_mismatch, etc.) come from the agent envelope
// and are preserved verbatim by classifyImportError.
const (
	errCodeImportTemplateNotFound = "template_not_found"
	errCodeImportPoolNotFound     = "pool_not_found"
	errCodeImportNodeNotFound     = "node_not_found"
	errCodeImportAgentUnreachable = "agent_unreachable"
	errCodeImportTimeout          = "import_timeout"
	errCodeImportInternal         = "internal"
)

// classifyImportError maps an executor error to a tasks.error.code. Agent
// envelope passthrough preserves agent-side codes verbatim (qcow2_header_invalid,
// checksum_mismatch, pool_full, ...). Network / 5xx failures collapse to
// `agent_unreachable`; poll-budget exhaustion collapses to `import_timeout`;
// everything else falls through to `internal`.
func classifyImportError(err error) string {
	var ae *agentclient.AgentError
	if errors.As(err, &ae) {
		if ae.Code != "" {
			return ae.Code
		}
		return errCodeImportAgentUnreachable
	}
	var te *agentclient.TimeoutError
	if errors.As(err, &te) {
		return errCodeImportTimeout
	}
	return errCodeImportInternal
}

// marshalImportResult serialises an ImportResult into the JSONB payload the
// OpenAPI Task.result field surfaces.
func marshalImportResult(r ImportResult) ([]byte, error) {
	return json.Marshal(r)
}
