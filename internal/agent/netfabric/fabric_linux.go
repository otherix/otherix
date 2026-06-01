// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package netfabric

import "net/netip"

// linuxFabric is the netlink-backed Fabric implementation. Bridge
// methods are implemented in bridge_linux.go; tap methods in
// tap_linux.go; NAT methods are placeholders until task 3.
type linuxFabric struct{}

// New returns the Linux netlink-backed Fabric.
func New() Fabric { return &linuxFabric{} }

// EnsureGatewayAddr is not yet implemented.
//
// TODO(T2/T3): real impl.
func (f *linuxFabric) EnsureGatewayAddr(bridge string, addr netip.Prefix) error {
	return errUnsupported
}

// RemoveGatewayAddr is not yet implemented.
//
// TODO(T2/T3): real impl.
func (f *linuxFabric) RemoveGatewayAddr(bridge string, addr netip.Prefix) error {
	return errUnsupported
}

// EnsureMasquerade is not yet implemented.
//
// TODO(T2/T3): real impl.
func (f *linuxFabric) EnsureMasquerade(subnet netip.Prefix, egressIface string) error {
	return errUnsupported
}

// RemoveMasquerade is not yet implemented.
//
// TODO(T2/T3): real impl.
func (f *linuxFabric) RemoveMasquerade(subnet netip.Prefix) error { return errUnsupported }
