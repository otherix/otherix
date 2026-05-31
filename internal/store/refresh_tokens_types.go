// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"net/netip"
	"time"

	"github.com/google/uuid"
)

type CreateRefreshTokenParams struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TokenHash []byte
	FamilyID  uuid.UUID
	ParentID  *uuid.UUID
	UserAgent *string
	IpAddress *netip.Addr
	ExpiresAt time.Time
}
