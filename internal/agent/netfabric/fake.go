// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package netfabric

import (
	"net"
	"net/netip"
)

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

	// ListTapsResult is returned by ListTaps alongside the configured
	// Errs["ListTaps"]. It is returned as nil when Errs["ListTaps"] is set.
	ListTapsResult []string

	EnsureBridgeCalls  []BridgeCall
	RemoveBridgeCalls  []string
	BridgeExistsCalls  []string
	CreateTapCalls     []TapCall
	AttachTapCalls     []AttachCall
	DeleteTapCalls     []string
	ListTapsCalls      int
	GatewayAddrCalls   []GatewayCall
	RemoveGatewayCalls []GatewayCall
	MasqueradeCalls    []MasqueradeCall
	RemoveMasqCalls    []RemoveMasqueradeCall

	MasqueradeIfaceCalls []MasqueradeIfaceCall
	RemoveMasqIfaceCalls []string

	BridgeRouteCalls       []BridgeRouteCall
	RemoveBridgeRouteCalls []BridgeRouteCall

	AnycastGatewayCalls       []AnycastGatewayCall
	RemoveAnycastGatewayCalls []AnycastGatewayCall

	EnsureVethCalls []VethConfig
	RemoveVethCalls []string

	// EnableIPForwardingCalls counts EnableIPForwarding invocations.
	EnableIPForwardingCalls int

	// VXLANExistsResult is returned by VXLANExists alongside
	// Errs["VXLANExists"].
	VXLANExistsResult bool

	EnsureVXLANCalls []VXLANConfig
	RemoveVXLANCalls []uint32
	VXLANExistsCalls []uint32

	// LinkStateResult maps an interface name to the LinkState that LinkState
	// returns; an absent name yields a zero LinkState. Errs["LinkState"] takes
	// precedence over the map.
	LinkStateResult map[string]LinkState
	LinkStateCalls  []string

	// FDBListResult is returned by FDBList alongside Errs["FDBList"]; nil
	// when the error is set.
	FDBListResult []FDBEntry

	FDBAppendCalls []FDBCall
	FDBDeleteCalls []FDBCall
	FDBListCalls   []uint32

	// FDBOpLog records "append" / "delete" in the exact order the methods were
	// called across both, so a test can assert their relative ordering (which the
	// separate FDBAppendCalls / FDBDeleteCalls slices cannot express).
	FDBOpLog []string

	// FDBOpLogMAC records "append:<mac>" / "delete:<mac>" in the same call order
	// as FDBOpLog but tagged with the entry's MAC, so a test can assert WHICH MAC
	// was appended or deleted (FDBOpLog alone cannot distinguish entries).
	FDBOpLogMAC []string

	// WireGuardExistsResult is returned by WireGuardExists alongside
	// Errs["WireGuardExists"].
	WireGuardExistsResult bool

	EnsureWireGuardCalls []WGConfig
	RemoveWireGuardCalls []string
	WireGuardExistsCalls []string

	// WireGuardPeersResult is returned by WireGuardPeers alongside
	// Errs["WireGuardPeers"]; nil when the error is set.
	WireGuardPeersResult []WGPeer

	// WireGuardPeerHandshakesResult is returned by WireGuardPeerHandshakes
	// alongside Errs["WireGuardPeerHandshakes"]; nil when the error is set.
	WireGuardPeerHandshakesResult []WGPeerHandshake
	WireGuardPeerHandshakesCalls  []string

	WGPeerSetCalls      []WGPeerSetCall
	WireGuardPeersCalls []string

	SendGARPCalls []SendGARPCall

	// NeighborResult maps NeighborKey(bridge, ip) to the outcome NeighborMAC
	// returns, letting a test program a guest IP to one MAC and then re-bind the
	// same IP to a different MAC - the IP-reuse anti-SSRF case. An absent key
	// yields the zero NeighborOutcome (unresolved). Errs["NeighborMAC"] takes
	// precedence over the map.
	NeighborResult   map[string]NeighborOutcome
	NeighborMACCalls []NeighborMACCall
}

// NeighborOutcome is the programmable result of a FakeFabric.NeighborMAC call.
type NeighborOutcome struct {
	MAC net.HardwareAddr
	OK  bool
	Err error
}

// NeighborMACCall records one NeighborMAC invocation.
type NeighborMACCall struct {
	Bridge string
	IP     netip.Addr
}

// NeighborKey builds the NeighborResult map key for a (bridge, ip) pair.
func NeighborKey(bridge string, ip netip.Addr) string {
	return bridge + "|" + ip.String()
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
	Bridge      string
	EgressIface string
}

// RemoveMasqueradeCall records one RemoveMasquerade invocation.
type RemoveMasqueradeCall struct {
	Subnet netip.Prefix
	Bridge string
}

// MasqueradeIfaceCall records one EnsureMasqueradeIface invocation.
type MasqueradeIfaceCall struct {
	InIface     string
	EgressIface string
}

// BridgeRouteCall records one EnsureBridgeRoute invocation.
type BridgeRouteCall struct {
	Subnet netip.Prefix
	Bridge string
}

// AnycastGatewayCall records one EnsureAnycastGateway or RemoveAnycastGateway
// invocation. MAC is the stringified hardware address (empty for removals).
type AnycastGatewayCall struct {
	Bridge string
	Addr   netip.Addr
	MAC    string
}

// SendGARPCall records one SendGARP invocation.
type SendGARPCall struct {
	Bridge string
	MAC    string
	IP     netip.Addr
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

// ListTaps records the call and returns ListTapsResult with
// Errs["ListTaps"]. When the error is non-nil the result is nil.
func (f *FakeFabric) ListTaps() ([]string, error) {
	f.ListTapsCalls++
	if err := f.err("ListTaps"); err != nil {
		return nil, err
	}
	return f.ListTapsResult, nil
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
func (f *FakeFabric) EnsureMasquerade(subnet netip.Prefix, bridge, egressIface string) error {
	f.MasqueradeCalls = append(f.MasqueradeCalls, MasqueradeCall{Subnet: subnet, Bridge: bridge, EgressIface: egressIface})
	return f.err("EnsureMasquerade")
}

// RemoveMasquerade records the call and returns
// Errs["RemoveMasquerade"].
func (f *FakeFabric) RemoveMasquerade(subnet netip.Prefix, bridge string) error {
	f.RemoveMasqCalls = append(f.RemoveMasqCalls, RemoveMasqueradeCall{Subnet: subnet, Bridge: bridge})
	return f.err("RemoveMasquerade")
}

// EnsureMasqueradeIface records the call and returns Errs["EnsureMasqueradeIface"].
func (f *FakeFabric) EnsureMasqueradeIface(inIface, egressIface string) error {
	f.MasqueradeIfaceCalls = append(f.MasqueradeIfaceCalls, MasqueradeIfaceCall{InIface: inIface, EgressIface: egressIface})
	return f.err("EnsureMasqueradeIface")
}

// RemoveMasqueradeIface records the call and returns Errs["RemoveMasqueradeIface"].
func (f *FakeFabric) RemoveMasqueradeIface(inIface string) error {
	f.RemoveMasqIfaceCalls = append(f.RemoveMasqIfaceCalls, inIface)
	return f.err("RemoveMasqueradeIface")
}

// EnsureBridgeRoute records the call and returns Errs["EnsureBridgeRoute"].
func (f *FakeFabric) EnsureBridgeRoute(subnet netip.Prefix, bridge string) error {
	f.BridgeRouteCalls = append(f.BridgeRouteCalls, BridgeRouteCall{Subnet: subnet, Bridge: bridge})
	return f.err("EnsureBridgeRoute")
}

// RemoveBridgeRoute records the call and returns Errs["RemoveBridgeRoute"].
func (f *FakeFabric) RemoveBridgeRoute(subnet netip.Prefix, bridge string) error {
	f.RemoveBridgeRouteCalls = append(f.RemoveBridgeRouteCalls, BridgeRouteCall{Subnet: subnet, Bridge: bridge})
	return f.err("RemoveBridgeRoute")
}

// EnableIPForwarding records the call and returns
// Errs["EnableIPForwarding"].
func (f *FakeFabric) EnableIPForwarding() error {
	f.EnableIPForwardingCalls++
	return f.err("EnableIPForwarding")
}

// EnsureAnycastGateway records the call and returns Errs["EnsureAnycastGateway"].
func (f *FakeFabric) EnsureAnycastGateway(bridge string, addr netip.Addr, mac net.HardwareAddr) error {
	f.AnycastGatewayCalls = append(f.AnycastGatewayCalls, AnycastGatewayCall{Bridge: bridge, Addr: addr, MAC: mac.String()})
	return f.err("EnsureAnycastGateway")
}

// RemoveAnycastGateway records the call and returns Errs["RemoveAnycastGateway"].
func (f *FakeFabric) RemoveAnycastGateway(bridge string, addr netip.Addr) error {
	f.RemoveAnycastGatewayCalls = append(f.RemoveAnycastGatewayCalls, AnycastGatewayCall{Bridge: bridge, Addr: addr})
	return f.err("RemoveAnycastGateway")
}

// EnsureVeth records the call and returns Errs["EnsureVeth"].
func (f *FakeFabric) EnsureVeth(cfg VethConfig) error {
	f.EnsureVethCalls = append(f.EnsureVethCalls, cfg)
	return f.err("EnsureVeth")
}

// RemoveVeth records the call and returns Errs["RemoveVeth"].
func (f *FakeFabric) RemoveVeth(host string) error {
	f.RemoveVethCalls = append(f.RemoveVethCalls, host)
	return f.err("RemoveVeth")
}

// EnsureVXLAN records the call and returns Errs["EnsureVXLAN"].
func (f *FakeFabric) EnsureVXLAN(cfg VXLANConfig) error {
	f.EnsureVXLANCalls = append(f.EnsureVXLANCalls, cfg)
	return f.err("EnsureVXLAN")
}

// RemoveVXLAN records the call and returns Errs["RemoveVXLAN"].
func (f *FakeFabric) RemoveVXLAN(vni uint32) error {
	f.RemoveVXLANCalls = append(f.RemoveVXLANCalls, vni)
	return f.err("RemoveVXLAN")
}

// VXLANExists records the call and returns VXLANExistsResult with
// Errs["VXLANExists"].
func (f *FakeFabric) VXLANExists(vni uint32) (bool, error) {
	f.VXLANExistsCalls = append(f.VXLANExistsCalls, vni)
	return f.VXLANExistsResult, f.err("VXLANExists")
}

// LinkState records the call and returns LinkStateResult[name] with
// Errs["LinkState"]. When the error is non-nil the result is the zero
// LinkState.
func (f *FakeFabric) LinkState(name string) (LinkState, error) {
	f.LinkStateCalls = append(f.LinkStateCalls, name)
	if err := f.err("LinkState"); err != nil {
		return LinkState{}, err
	}
	return f.LinkStateResult[name], nil
}

// FDBCall records one FDBAppend or FDBDelete invocation.
type FDBCall struct {
	VNI   uint32
	Entry FDBEntry
}

// FDBAppend records the call and returns Errs["FDBAppend"].
func (f *FakeFabric) FDBAppend(vni uint32, e FDBEntry) error {
	f.FDBAppendCalls = append(f.FDBAppendCalls, FDBCall{VNI: vni, Entry: e})
	f.FDBOpLog = append(f.FDBOpLog, "append")
	f.FDBOpLogMAC = append(f.FDBOpLogMAC, "append:"+e.MAC.String())
	return f.err("FDBAppend")
}

// FDBDelete records the call and returns Errs["FDBDelete"].
func (f *FakeFabric) FDBDelete(vni uint32, e FDBEntry) error {
	f.FDBDeleteCalls = append(f.FDBDeleteCalls, FDBCall{VNI: vni, Entry: e})
	f.FDBOpLog = append(f.FDBOpLog, "delete")
	f.FDBOpLogMAC = append(f.FDBOpLogMAC, "delete:"+e.MAC.String())
	return f.err("FDBDelete")
}

// FDBList records the call and returns FDBListResult with Errs["FDBList"].
// When the error is non-nil the result is nil.
func (f *FakeFabric) FDBList(vni uint32) ([]FDBEntry, error) {
	f.FDBListCalls = append(f.FDBListCalls, vni)
	if err := f.err("FDBList"); err != nil {
		return nil, err
	}
	return f.FDBListResult, nil
}

// EnsureWireGuard records the call and returns Errs["EnsureWireGuard"].
func (f *FakeFabric) EnsureWireGuard(cfg WGConfig) error {
	f.EnsureWireGuardCalls = append(f.EnsureWireGuardCalls, cfg)
	return f.err("EnsureWireGuard")
}

// RemoveWireGuard records the call and returns Errs["RemoveWireGuard"].
func (f *FakeFabric) RemoveWireGuard(name string) error {
	f.RemoveWireGuardCalls = append(f.RemoveWireGuardCalls, name)
	return f.err("RemoveWireGuard")
}

// WireGuardExists records the call and returns WireGuardExistsResult with
// Errs["WireGuardExists"].
func (f *FakeFabric) WireGuardExists(name string) (bool, error) {
	f.WireGuardExistsCalls = append(f.WireGuardExistsCalls, name)
	return f.WireGuardExistsResult, f.err("WireGuardExists")
}

// WGPeerSetCall records one SetWireGuardPeers invocation.
type WGPeerSetCall struct {
	Name  string
	Peers []WGPeer
}

// SetWireGuardPeers records the call and returns Errs["SetWireGuardPeers"].
func (f *FakeFabric) SetWireGuardPeers(name string, peers []WGPeer) error {
	f.WGPeerSetCalls = append(f.WGPeerSetCalls, WGPeerSetCall{Name: name, Peers: peers})
	return f.err("SetWireGuardPeers")
}

// WireGuardPeers records the call and returns WireGuardPeersResult with
// Errs["WireGuardPeers"]. When the error is non-nil the result is nil.
func (f *FakeFabric) WireGuardPeers(name string) ([]WGPeer, error) {
	f.WireGuardPeersCalls = append(f.WireGuardPeersCalls, name)
	if err := f.err("WireGuardPeers"); err != nil {
		return nil, err
	}
	return f.WireGuardPeersResult, nil
}

// WireGuardPeerHandshakes records the call and returns
// WireGuardPeerHandshakesResult with Errs["WireGuardPeerHandshakes"]. When the
// error is non-nil the result is nil.
func (f *FakeFabric) WireGuardPeerHandshakes(name string) ([]WGPeerHandshake, error) {
	f.WireGuardPeerHandshakesCalls = append(f.WireGuardPeerHandshakesCalls, name)
	if err := f.err("WireGuardPeerHandshakes"); err != nil {
		return nil, err
	}
	return f.WireGuardPeerHandshakesResult, nil
}

// SendGARP records the call and returns the configured Errs["SendGARP"].
func (f *FakeFabric) SendGARP(bridge string, mac string, ip netip.Addr) error {
	f.SendGARPCalls = append(f.SendGARPCalls, SendGARPCall{Bridge: bridge, MAC: mac, IP: ip})
	return f.err("SendGARP")
}

// NeighborMAC records the call and returns the programmed NeighborResult for
// (bridge, ip). Errs["NeighborMAC"] takes precedence and yields an error result.
func (f *FakeFabric) NeighborMAC(bridge string, ip netip.Addr) (net.HardwareAddr, bool, error) {
	f.NeighborMACCalls = append(f.NeighborMACCalls, NeighborMACCall{Bridge: bridge, IP: ip})
	if err := f.err("NeighborMAC"); err != nil {
		return nil, false, err
	}
	o := f.NeighborResult[NeighborKey(bridge, ip)]
	return o.MAC, o.OK, o.Err
}

// Ensure FakeFabric satisfies Fabric at compile time.
var _ Fabric = (*FakeFabric)(nil)
