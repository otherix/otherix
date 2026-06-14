// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

// CreateMigrationParams is the input to Store.CreateMigration.
type CreateMigrationParams struct {
	ID                uuid.UUID
	VmID              uuid.UUID
	SourceNodeID      *uuid.UUID
	TargetNodeID      *uuid.UUID
	TargetPoolName    string
	InitiatedByUserID *uuid.UUID
	Reason            MigrationReason
	Live              bool
	AllowPostcopy     bool
	MaxBandwidthBytes *int64
	MaxDowntimeMs     *int32
	Task              CreateTaskParams
}

// MigrationProgressUpdate is the set of mutable migration columns an in-flight
// update may touch. Every field is a pointer (or, for SchedulingReason, paired
// with ClearSchedulingReason) so a nil leaves the column untouched - the store
// applies only the non-nil fields under a single ModRevision CAS. Phase may move
// the migration to a terminal failed/cancelled, which releases its transient
// locks; cutover to completed is a separate dedicated path.
type MigrationProgressUpdate struct {
	Phase                 *MigrationPhase
	ProgressPercent       *int16
	BytesTotal            *int64
	BytesTransferred      *int64
	BytesRemaining        *int64
	ErrorMessage          *string
	StartedAt             *time.Time
	LastScheduleAttemptAt *time.Time
	// SchedulingReason sets the retryable scheduling reason. To CLEAR it instead,
	// set ClearSchedulingReason (a nil SchedulingReason with ClearSchedulingReason
	// false leaves the column untouched).
	SchedulingReason      *string
	ClearSchedulingReason bool
}

// ListMigrationsParams is cursor pagination plus optional VM/node filters.
type ListMigrationsParams struct {
	LimitCount      int32
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	VmID            *uuid.UUID
	NodeID          *uuid.UUID
}
