// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// Join-token kinds gate which redemption endpoint a token may use. A node
// token (the default, also the back-compat reading of an empty Kind on an
// older row) redeems at /v1/nodes/join for a leaf cert; a gateway token also
// redeems at /v1/nodes/join but self-registers an ingress gateway, yielding a
// unified node-<name> leaf and a gateway-role node row; a cluster token redeems at
// /v1/cluster/join for the CA cert + key (a joining CP replica). The privilege
// is intrinsic to the token, fixed at mint - never a request flag - so a stolen
// node token can never yield the CA key.
const (
	JoinTokenKindNode    = "node"
	JoinTokenKindGateway = "gateway"
	JoinTokenKindCluster = "cluster"
)

type CreateJoinTokenParams struct {
	ID               uuid.UUID
	TokenHash        []byte
	Kind             string
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
	Kind             string
	IntendedNodeName *string
	CreatedByUserID  *uuid.UUID
	ExpiresAt        time.Time
	MaxUses          *int32
	CreatedAt        time.Time
	ConsumptionCount int64
}
