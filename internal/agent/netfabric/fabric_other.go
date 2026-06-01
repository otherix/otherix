// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build !linux
// +build !linux

package netfabric

import (
	"errors"
	"net/netip"
)

// errUnsupported is returned by every method on non-Linux builds.
// Callers may errors.Is against it to detect that the platform does not
// support host networking.
var errUnsupported = errors.New("netfabric: unsupported on this platform")

// unsupportedFabric is the non-Linux Fabric stub. The agent is
// Linux-only; this implementation exists so the agent cross-compiles and
// unit-tests on developer machines. Every method returns errUnsupported;
// none of them runs in a live agent process.
type unsupportedFabric struct{}

var _ Fabric = unsupportedFabric{}

// New returns a Fabric whose every method reports errUnsupported.
func New() Fabric { return unsupportedFabric{} }

// EnsureBridge reports errUnsupported on non-Linux builds.
func (unsupportedFabric) EnsureBridge(name string, mtu int) error { return errUnsupported }

// RemoveBridge reports errUnsupported on non-Linux builds.
func (unsupportedFabric) RemoveBridge(name string) error { return errUnsupported }

// BridgeExists reports errUnsupported on non-Linux builds.
func (unsupportedFabric) BridgeExists(name string) (bool, error) { return false, errUnsupported }

// CreateTap reports errUnsupported on non-Linux builds.
func (unsupportedFabric) CreateTap(name string, mtu int) error { return errUnsupported }

// AttachTap reports errUnsupported on non-Linux builds.
func (unsupportedFabric) AttachTap(tap, bridge string) error { return errUnsupported }

// DeleteTap reports errUnsupported on non-Linux builds.
func (unsupportedFabric) DeleteTap(name string) error { return errUnsupported }

// EnsureGatewayAddr reports errUnsupported on non-Linux builds.
func (unsupportedFabric) EnsureGatewayAddr(bridge string, addr netip.Prefix) error {
	return errUnsupported
}

// RemoveGatewayAddr reports errUnsupported on non-Linux builds.
func (unsupportedFabric) RemoveGatewayAddr(bridge string, addr netip.Prefix) error {
	return errUnsupported
}

// EnsureMasquerade reports errUnsupported on non-Linux builds.
func (unsupportedFabric) EnsureMasquerade(subnet netip.Prefix, egressIface string) error {
	return errUnsupported
}

// RemoveMasquerade reports errUnsupported on non-Linux builds.
func (unsupportedFabric) RemoveMasquerade(subnet netip.Prefix) error { return errUnsupported }
