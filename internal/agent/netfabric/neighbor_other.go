// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build !linux

package netfabric

import (
	"net"
	"net/netip"
)

// NeighborMAC is linux-only; the stub keeps the package buildable on darwin (dev).
func (unsupportedFabric) NeighborMAC(bridge string, ip netip.Addr) (net.HardwareAddr, bool, error) {
	return nil, false, errUnsupported
}
