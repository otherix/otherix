// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

type CreateTaskParams struct {
	ID           uuid.UUID
	Type         string
	Status       TaskStatus
	ResourceType string
	ResourceID   *uuid.UUID
	Args         []byte
	MaxAttempts  int32
	CreatedBy    *uuid.UUID
}

type DeleteExpiredTasksParams struct {
	CompletedCutoff time.Time
	FailedCutoff    time.Time
}

type ListTasksAnyParams struct {
	StatusFilter       *string
	TypeFilter         *string
	ResourceTypeFilter *string
	ResourceIDFilter   *uuid.UUID
	CursorCreatedAt    *time.Time
	CursorID           *uuid.UUID
	LimitCount         int32
}

type ListTasksOwnParams struct {
	CreatedBy          *uuid.UUID
	StatusFilter       *string
	TypeFilter         *string
	ResourceTypeFilter *string
	ResourceIDFilter   *uuid.UUID
	CursorCreatedAt    *time.Time
	CursorID           *uuid.UUID
	LimitCount         int32
}

type UpdateTaskAgentTaskIDParams struct {
	AgentTaskID *uuid.UUID
	ID          uuid.UUID
}

type UpdateTaskFinalizedParams struct {
	Status TaskStatus
	Result []byte
	Error  []byte
	ID     uuid.UUID
}
