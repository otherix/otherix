// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package netfabric

import "net/netip"

// linuxFabric is the netlink-backed Fabric implementation. Bridge
// methods are implemented in bridge_linux.go; tap and NAT methods are
// placeholders until tasks 2 and 3.
type linuxFabric struct{}

// New returns the Linux netlink-backed Fabric.
func New() Fabric { return &linuxFabric{} }

// CreateTap is not yet implemented.
//
// TODO(T2/T3): real impl.
func (f *linuxFabric) CreateTap(name string, mtu int) error { return errUnsupported }

// AttachTap is not yet implemented.
//
// TODO(T2/T3): real impl.
func (f *linuxFabric) AttachTap(tap, bridge string) error { return errUnsupported }

// DeleteTap is not yet implemented.
//
// TODO(T2/T3): real impl.
func (f *linuxFabric) DeleteTap(name string) error { return errUnsupported }

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
