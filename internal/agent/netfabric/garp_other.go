// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build !linux

package netfabric

import (
	"net/netip"
)

// SendGARP is linux-only; the stub keeps the package buildable on darwin (dev).
func (unsupportedFabric) SendGARP(bridge string, mac string, ip netip.Addr) error {
	return errUnsupported
}
