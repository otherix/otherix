// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package netid mints layer-2/3 identifiers the control plane assigns to guest
// and gateway interfaces. It is dependency-free so both the API handlers and the
// store can share one generator without crossing the pgx-free type-layer
// boundary.
package netid

import (
	"crypto/rand"
	"net"
)

// GenerateLocalMAC mints a locally-administered unicast MAC in QEMU's 52:54:00
// OUI with three random low bytes. The 52:54:00 prefix is the conventional
// QEMU/KVM range; the random suffix gives ~16M values, so a collision within a
// cluster is astronomically unlikely and no retry loop is warranted. Both the
// VM-NIC bind and the gateway-membership allocation draw their MAC from here so
// the two share one address convention.
func GenerateLocalMAC() (net.HardwareAddr, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	return net.HardwareAddr{0x52, 0x54, 0x00, b[0], b[1], b[2]}, nil
}
