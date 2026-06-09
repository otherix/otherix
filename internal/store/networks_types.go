// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"net/netip"
	"time"

	"github.com/google/uuid"
)

type CreateNetworkParams struct {
	ID          uuid.UUID
	Name        string
	Type        NetworkType
	BridgeName  string
	Managed     bool
	Egress      NetworkEgress
	VlanTag     *int32
	Mtu         int32
	Subnet      *netip.Prefix
	Gateway     *netip.Addr
	DhcpEnabled bool
	Config      []byte
}

type ListNetworksParams struct {
	Type            *NetworkType
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}

type UpdateNetworkParams struct {
	Name       string
	BridgeName string
	Managed    bool
	Egress     NetworkEgress
	VlanTag    *int32
	Mtu        int32
	Subnet     *netip.Prefix
	Gateway    *netip.Addr
	Config     []byte
	ID         uuid.UUID
}
