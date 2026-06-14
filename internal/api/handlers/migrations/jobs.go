// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package migrations hosts the /v1/vms/{id}/migrate + /v1/migrations/* HTTP
// handlers and the vm.migrate queue contract.
package migrations

import (
	"context"

	"github.com/google/uuid"
)

// This file holds the queue contract for the vm.migrate task: the job-arg
// payload, the executor seam the worker drives the agent migration through, and
// the slice-wide scheduling-reason / terminal error-code catalogue. The worker
// implementation that consumes this contract (the agent QMP handshake, the
// pre-copy / cutover drive loop) lands in a later task and owns the concrete
// executor; this file declares only the contract.

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

// MigrationExecutor is the per-task-type seam the worker drives a migration
// through. The production implementation (agent QMP handshake over agentclient)
// lands with the worker in a later task; tests use an in-package fake.
//
// The seam is intentionally minimal: the worker resolves the migration row by
// id and drives it to a terminal phase (the cutover commit / fail / cancel
// transitions are store methods, not part of this seam). It is honest about
// what is known now - a single drive entry point keyed on the migration id - and
// is expected to be refined when the agent data-path shape is settled.
type MigrationExecutor interface {
	// RunMigration drives the migration with the given id to a terminal outcome,
	// returning an error when the migration could not be completed. The worker
	// owns the store transitions; the executor owns the agent-side handshake.
	RunMigration(ctx context.Context, migrationID uuid.UUID) error
}

// Scheduling-reason constants for the migration row's SchedulingReason field
// while it is pending and no target is bound (mirrors VM.SchedulingReason).
const (
	ReasonNoEligibleTarget = "no_eligible_target"
	ReasonNoCapacity       = "no_capacity"
	ReasonPoolNotReady     = "pool_not_ready"
)

// Terminal error-code constants for the task `error` JSONB envelope.
const (
	ErrCodeTargetUnreachable = "target_unreachable"
	ErrCodeTLSHandshake      = "tls_handshake_failed"
	ErrCodeConvergenceFailed = "convergence_failed"
)
