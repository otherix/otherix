// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package netfabric

// linuxFabric is the netlink-backed Fabric implementation. Bridge
// methods are implemented in bridge_linux.go; tap methods in
// tap_linux.go; gateway-address and masquerade methods in nat_linux.go.
type linuxFabric struct{}

var _ Fabric = (*linuxFabric)(nil)

// New returns the Linux netlink-backed Fabric.
func New() Fabric { return &linuxFabric{} }
