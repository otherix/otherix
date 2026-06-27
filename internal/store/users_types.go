// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

type CountUserResourcesRow struct {
	Vms       int64
	Snapshots int64
}

type CreateUserParams struct {
	ID           uuid.UUID
	Username     string
	Email        string // optional; "" means absent
	PasswordHash string
	DisplayName  string
	Role         string
}

type ListUsersParams struct {
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}

type UpdateUserParams struct {
	Email        string
	PasswordHash string
	DisplayName  string
	Role         string
	ID           uuid.UUID
}
