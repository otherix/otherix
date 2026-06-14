// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package migrations hosts the /v1/vms/{id}/migrate + /v1/migrations/* HTTP
// handlers and the vm.migrate queue contract.
package migrations

import (
	"github.com/google/uuid"
)

// This file holds the queue contract for the vm.migrate task: the job-arg
// payload and the slice-wide scheduling-reason / terminal error-code catalogue.
// The worker that consumes this contract (placement, the two-phase agent
// handshake, the cutover drive loop) lives in run.go and defines its own
// store / agent-client seams; this file declares only the queue payload + the
// shared reason/error codes.

// MigrationRunArgs is the queue job-args payload for a `vm.migrate` task. The
// atomic-enqueue path inserts the migration row and its backing task, picks a
// fresh task id, and enqueues this payload; the worker reads task / migration
// ids back to resume the migration saga it observed at hand-off. Both ids are
// durable references - the worker resolves the migration row (target, options,
// agent_task_id) from MigrationID at execution time rather than carrying that
// detail on the queue.
type MigrationRunArgs struct {
	TaskID      uuid.UUID `json:"task_id"`
	MigrationID uuid.UUID `json:"migration_id"`
}

// Kind names the job kind. Mirrors the OpenAPI Task.type value surfaced through
// tasks.{list,get}.
func (MigrationRunArgs) Kind() string { return "vm.migrate" }

// Scheduling-reason constants for the migration row's SchedulingReason field
// while it is pending and no target is bound (mirrors VM.SchedulingReason).
const (
	ReasonNoEligibleTarget = "no_eligible_target"
	ReasonNoCapacity       = "no_capacity"
	ReasonPoolNotReady     = "pool_not_ready"
)

// Terminal error-code constants for the task `error` JSONB envelope.
const (
	ErrCodeTargetUnreachable  = "target_unreachable"
	ErrCodeTLSHandshake       = "tls_handshake_failed"
	ErrCodeConvergenceFailed  = "convergence_failed"
	ErrCodeMigrationCancelled = "migration_cancelled"
)
