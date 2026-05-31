// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

type CreateApiTokenParams struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Name      string
	TokenHash []byte
	Prefix    string
	ExpiresAt *time.Time
}

type ListApiTokensByUserParams struct {
	UserID          uuid.UUID
	IncludeRevoked  bool
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}
