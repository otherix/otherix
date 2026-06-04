// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"net"
	"net/netip"

	"github.com/google/uuid"
)

// CreateVMNicParams carries the inputs for a single VM network interface row.
// MacAddress is the locally-administered QEMU MAC the CP mints at create time;
// Ipv4Address/Ipv6Address are nil in the bridge-attach slice (the guest
// configures its own addressing).
type CreateVMNicParams struct {
	ID          uuid.UUID
	VmID        uuid.UUID
	NetworkID   uuid.UUID
	DeviceOrder int32
	Model       NicModel
	MacAddress  net.HardwareAddr
	Ipv4Address *netip.Addr
	Ipv6Address *netip.Addr
}
