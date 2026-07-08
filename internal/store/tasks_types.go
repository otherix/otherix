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
	// Idempotency descriptor, set only for opt-in mutating requests that carry an
	// Idempotency-Key. When all three are non-nil, EnqueueTask commits a
	// (user,key)->{task_id,hash} index under a CreateRevision guard so a redelivery
	// or middleware re-run returns the existing task instead of a second one.
	IdempotencyUserID *uuid.UUID
	IdempotencyKey    *string
	IdempotencyHash   []byte
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
