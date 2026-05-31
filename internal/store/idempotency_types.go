// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

type BeginIdempotencyKeyParams struct {
	Key           string
	UserID        *uuid.UUID
	RequestMethod string
	RequestPath   string
	RequestHash   []byte
	ExpiresAt     time.Time
}

type CompleteIdempotencyKeyParams struct {
	ResponseStatus  *int32
	ResponseHeaders []byte
	ResponseBody    []byte
	Key             string
}

type ReclaimIdempotencyKeyParams struct {
	UserID        *uuid.UUID
	RequestMethod string
	RequestPath   string
	RequestHash   []byte
	ExpiresAt     time.Time
	Key           string
}
