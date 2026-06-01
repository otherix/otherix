// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package netfabric

import "sync"

// linuxFabric is the netlink-backed Fabric implementation. Bridge
// methods are implemented in bridge_linux.go; tap methods in
// tap_linux.go; gateway-address and masquerade methods in nat_linux.go.
type linuxFabric struct {
	// natMu serialises the NAT and gateway-address mutators. Each of those
	// methods opens its own nftables.Conn and runs a
	// GetRules-precheck-then-AddRule sequence that is not atomic across
	// concurrent callers; the mutex keeps two goroutines from racing on the
	// shared kernel table.
	natMu sync.Mutex
}

var _ Fabric = (*linuxFabric)(nil)

// New returns the Linux netlink-backed Fabric.
func New() Fabric { return &linuxFabric{} }
