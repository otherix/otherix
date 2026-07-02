// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build !linux
// +build !linux

package netfabric

import (
	"errors"
	"net"
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

// ListTaps reports errUnsupported on non-Linux builds.
func (unsupportedFabric) ListTaps() ([]string, error) { return nil, errUnsupported }

// EnsureGatewayAddr reports errUnsupported on non-Linux builds.
func (unsupportedFabric) EnsureGatewayAddr(bridge string, addr netip.Prefix) error {
	return errUnsupported
}

// RemoveGatewayAddr reports errUnsupported on non-Linux builds.
func (unsupportedFabric) RemoveGatewayAddr(bridge string, addr netip.Prefix) error {
	return errUnsupported
}

// EnsureMasquerade reports errUnsupported on non-Linux builds.
func (unsupportedFabric) EnsureMasquerade(subnet netip.Prefix, bridge, egressIface string) error {
	return errUnsupported
}

// RemoveMasquerade reports errUnsupported on non-Linux builds.
func (unsupportedFabric) RemoveMasquerade(subnet netip.Prefix, bridge string) error {
	return errUnsupported
}

// EnableIPForwarding reports errUnsupported on non-Linux builds.
func (unsupportedFabric) EnableIPForwarding() error { return errUnsupported }

// EnsureAnycastGateway reports errUnsupported on non-Linux builds.
func (unsupportedFabric) EnsureAnycastGateway(string, netip.Addr, net.HardwareAddr) error {
	return errUnsupported
}

// RemoveAnycastGateway reports errUnsupported on non-Linux builds.
func (unsupportedFabric) RemoveAnycastGateway(string, netip.Addr) error { return errUnsupported }

// EnsureUnicastGateway reports errUnsupported on non-Linux builds.
func (unsupportedFabric) EnsureUnicastGateway(string, netip.Prefix, net.HardwareAddr) error {
	return errUnsupported
}

// EnsureVeth reports errUnsupported on non-Linux builds.
func (unsupportedFabric) EnsureVeth(cfg VethConfig) error { return errUnsupported }

// RemoveVeth reports errUnsupported on non-Linux builds.
func (unsupportedFabric) RemoveVeth(host string) error { return errUnsupported }

// EnsureMasqueradeIface reports errUnsupported on non-Linux builds.
func (unsupportedFabric) EnsureMasqueradeIface(string, string) error { return errUnsupported }

// RemoveMasqueradeIface reports errUnsupported on non-Linux builds.
func (unsupportedFabric) RemoveMasqueradeIface(string) error { return errUnsupported }

// EnsureBridgeRoute reports errUnsupported on non-Linux builds.
func (unsupportedFabric) EnsureBridgeRoute(netip.Prefix, string) error { return errUnsupported }

// RemoveBridgeRoute reports errUnsupported on non-Linux builds.
func (unsupportedFabric) RemoveBridgeRoute(netip.Prefix, string) error { return errUnsupported }

// EnsureVXLAN reports errUnsupported on non-Linux builds.
func (unsupportedFabric) EnsureVXLAN(cfg VXLANConfig) error { return errUnsupported }

// RemoveVXLAN reports errUnsupported on non-Linux builds.
func (unsupportedFabric) RemoveVXLAN(vni uint32) error { return errUnsupported }

// VXLANExists reports errUnsupported on non-Linux builds.
func (unsupportedFabric) VXLANExists(vni uint32) (bool, error) { return false, errUnsupported }

// FDBAppend reports errUnsupported on non-Linux builds.
func (unsupportedFabric) FDBAppend(vni uint32, e FDBEntry) error { return errUnsupported }

// FDBDelete reports errUnsupported on non-Linux builds.
func (unsupportedFabric) FDBDelete(vni uint32, e FDBEntry) error { return errUnsupported }

// FDBList reports errUnsupported on non-Linux builds.
func (unsupportedFabric) FDBList(vni uint32) ([]FDBEntry, error) { return nil, errUnsupported }

// LinkState reports errUnsupported on non-Linux builds.
func (unsupportedFabric) LinkState(name string) (LinkState, error) {
	return LinkState{}, errUnsupported
}

// EnsureWireGuard reports errUnsupported on non-Linux builds.
func (unsupportedFabric) EnsureWireGuard(cfg WGConfig) error { return errUnsupported }

// RemoveWireGuard reports errUnsupported on non-Linux builds.
func (unsupportedFabric) RemoveWireGuard(name string) error { return errUnsupported }

// WireGuardExists reports errUnsupported on non-Linux builds.
func (unsupportedFabric) WireGuardExists(name string) (bool, error) { return false, errUnsupported }

// SetWireGuardPeers reports errUnsupported on non-Linux builds.
func (unsupportedFabric) SetWireGuardPeers(name string, peers []WGPeer) error { return errUnsupported }

// WireGuardPeers reports errUnsupported on non-Linux builds.
func (unsupportedFabric) WireGuardPeers(name string) ([]WGPeer, error) { return nil, errUnsupported }

// WireGuardPeerHandshakes reports errUnsupported on non-Linux builds.
func (unsupportedFabric) WireGuardPeerHandshakes(name string) ([]WGPeerHandshake, error) {
	return nil, errUnsupported
}
