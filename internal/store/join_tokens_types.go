// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"net/netip"
	"time"

	"github.com/google/uuid"
)

type CreateJoinTokenParams struct {
	ID               uuid.UUID
	TokenHash        []byte
	IntendedNodeName *string
	CreatedByUserID  *uuid.UUID
	ExpiresAt        time.Time
	MaxUses          *int32
}

type CreateJoinTokenConsumptionParams struct {
	ID               uuid.UUID
	JoinTokenID      uuid.UUID
	ConsumedByNodeID *uuid.UUID
	SourceIp         *netip.Addr
}

type ListJoinTokenConsumptionsParams struct {
	JoinTokenID      uuid.UUID
	CursorConsumedAt *time.Time
	CursorID         *uuid.UUID
	LimitCount       int32
}

type ListJoinTokensParams struct {
	IncludeExpired  bool
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}

type ListJoinTokensRow struct {
	ID               uuid.UUID
	TokenHash        []byte
	IntendedNodeName *string
	CreatedByUserID  *uuid.UUID
	ExpiresAt        time.Time
	MaxUses          *int32
	CreatedAt        time.Time
	ConsumptionCount int64
}
