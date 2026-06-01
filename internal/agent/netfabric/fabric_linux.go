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
	// mu serialises every mutating method. One fabric instance is shared
	// between the network reconciler goroutine (bridge / NAT ops) and the
	// VM-manager handler goroutines (tap ops), so the mutators must not run
	// concurrently: the nft mutators run a non-atomic
	// GetRules-precheck-then-AddRule sequence against the shared kernel
	// table, and an AttachTap racing a RemoveBridge can fail recoverably.
	// The single mutex makes the fabric safe for concurrent use. Read-only
	// methods (BridgeExists, VXLANExists, ListTaps, FDBList) do not lock;
	// none of them calls a mutator, so locking the mutators alone cannot
	// self-deadlock.
	mu sync.Mutex
}

var _ Fabric = (*linuxFabric)(nil)

// New returns the Linux netlink-backed Fabric.
func New() Fabric { return &linuxFabric{} }
