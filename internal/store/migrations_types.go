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

// ListMigrationsParams is cursor pagination plus optional VM/node filters.
type ListMigrationsParams struct {
	LimitCount      int32
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	VmID            *uuid.UUID
	NodeID          *uuid.UUID
}
