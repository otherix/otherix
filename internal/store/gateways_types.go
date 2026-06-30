// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"net"
	"net/netip"
	"time"

	"github.com/google/uuid"
)

// NodeKind discriminates the role a node row plays in the cluster. A node-kind
// row is a hypervisor that hosts VMs and is a candidate for VM placement; a
// gateway-kind row terminates ingress traffic and never hosts VMs, so the
// scheduler excludes it from placement. An empty Kind on an older row reads back
// as the node kind.
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
