// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"net"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// NodeKindGateway is the legacy node-kind discriminator for an ingress gateway.
// Retained as the sentinel Node.UnmarshalJSON migrates from when reading a
// pre-roles node row (Kind=="gateway" -> GatewayRole=true). No production code
// writes it. NodeKindNode is its non-gateway counterpart, used only by tests.
const (
	NodeKindNode    = "node"
	NodeKindGateway = "gateway"
)

// GatewayMembership records that a gateway node covers an overlay network. The
// gateway is given a TenantIP and MAC drawn from the same per-network address
// space as VM NICs, so a gateway and a VM can never collide on an address. VNI
// is copied from the network at create time for the agent-facing projection.
type GatewayMembership struct {
	GatewayID uuid.UUID
	NetworkID uuid.UUID
	VNI       int32
	MAC       net.HardwareAddr
	TenantIP  netip.Addr
	CreatedAt time.Time
}
