// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package netfabric

import "net/netip"

// FakeFabric is a Spy implementation of Fabric for tests. It records
// every call into an exported slice and returns a configurable error
// per method via Errs, keyed by method name (nil or absent => success).
// It pulls in no netlink dependency, so it compiles and runs on every
// platform. FakeFabric is not safe for concurrent use.
type FakeFabric struct {
	// Errs maps a method name (e.g. "EnsureBridge", "CreateTap") to the
	// error that method returns. A nil or absent entry means success.
	Errs map[string]error

	// BridgeExistsResult is returned by BridgeExists alongside the
	// configured Errs["BridgeExists"].
	BridgeExistsResult bool

	EnsureBridgeCalls  []BridgeCall
	RemoveBridgeCalls  []string
	BridgeExistsCalls  []string
	CreateTapCalls     []TapCall
	AttachTapCalls     []AttachCall
	DeleteTapCalls     []string
	GatewayAddrCalls   []GatewayCall
	RemoveGatewayCalls []GatewayCall
	MasqueradeCalls    []MasqueradeCall
	RemoveMasqCalls    []netip.Prefix
}

// BridgeCall records one EnsureBridge invocation.
type BridgeCall struct {
	Name string
	MTU  int
}

// TapCall records one CreateTap invocation.
type TapCall struct {
	Name string
	MTU  int
}

// AttachCall records one AttachTap invocation.
type AttachCall struct {
	Tap    string
	Bridge string
}

// GatewayCall records one EnsureGatewayAddr or RemoveGatewayAddr
// invocation.
type GatewayCall struct {
	Bridge string
	Addr   netip.Prefix
}

// MasqueradeCall records one EnsureMasquerade invocation.
type MasqueradeCall struct {
	Subnet      netip.Prefix
	EgressIface string
}

func (f *FakeFabric) err(method string) error {
	if f.Errs == nil {
		return nil
	}
	return f.Errs[method]
}

// EnsureBridge records the call and returns Errs["EnsureBridge"].
func (f *FakeFabric) EnsureBridge(name string, mtu int) error {
	f.EnsureBridgeCalls = append(f.EnsureBridgeCalls, BridgeCall{Name: name, MTU: mtu})
	return f.err("EnsureBridge")
}

// RemoveBridge records the call and returns Errs["RemoveBridge"].
func (f *FakeFabric) RemoveBridge(name string) error {
	f.RemoveBridgeCalls = append(f.RemoveBridgeCalls, name)
	return f.err("RemoveBridge")
}

// BridgeExists records the call and returns BridgeExistsResult with
// Errs["BridgeExists"].
func (f *FakeFabric) BridgeExists(name string) (bool, error) {
	f.BridgeExistsCalls = append(f.BridgeExistsCalls, name)
	return f.BridgeExistsResult, f.err("BridgeExists")
}

// CreateTap records the call and returns Errs["CreateTap"].
func (f *FakeFabric) CreateTap(name string, mtu int) error {
	f.CreateTapCalls = append(f.CreateTapCalls, TapCall{Name: name, MTU: mtu})
	return f.err("CreateTap")
}

// AttachTap records the call and returns Errs["AttachTap"].
func (f *FakeFabric) AttachTap(tap, bridge string) error {
	f.AttachTapCalls = append(f.AttachTapCalls, AttachCall{Tap: tap, Bridge: bridge})
	return f.err("AttachTap")
}

// DeleteTap records the call and returns Errs["DeleteTap"].
func (f *FakeFabric) DeleteTap(name string) error {
	f.DeleteTapCalls = append(f.DeleteTapCalls, name)
	return f.err("DeleteTap")
}

// EnsureGatewayAddr records the call and returns
// Errs["EnsureGatewayAddr"].
func (f *FakeFabric) EnsureGatewayAddr(bridge string, addr netip.Prefix) error {
	f.GatewayAddrCalls = append(f.GatewayAddrCalls, GatewayCall{Bridge: bridge, Addr: addr})
	return f.err("EnsureGatewayAddr")
}

// RemoveGatewayAddr records the call and returns
// Errs["RemoveGatewayAddr"].
func (f *FakeFabric) RemoveGatewayAddr(bridge string, addr netip.Prefix) error {
	f.RemoveGatewayCalls = append(f.RemoveGatewayCalls, GatewayCall{Bridge: bridge, Addr: addr})
	return f.err("RemoveGatewayAddr")
}

// EnsureMasquerade records the call and returns
// Errs["EnsureMasquerade"].
func (f *FakeFabric) EnsureMasquerade(subnet netip.Prefix, egressIface string) error {
	f.MasqueradeCalls = append(f.MasqueradeCalls, MasqueradeCall{Subnet: subnet, EgressIface: egressIface})
	return f.err("EnsureMasquerade")
}

// RemoveMasquerade records the call and returns
// Errs["RemoveMasquerade"].
func (f *FakeFabric) RemoveMasquerade(subnet netip.Prefix) error {
	f.RemoveMasqCalls = append(f.RemoveMasqCalls, subnet)
	return f.err("RemoveMasquerade")
}

// Ensure FakeFabric satisfies Fabric at compile time.
var _ Fabric = (*FakeFabric)(nil)
