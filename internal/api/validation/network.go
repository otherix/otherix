// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"

	"github.com/otherix/otherix/internal/store"
)

// NetworkNameMaxLength bounds networks.name and matches
// `Network.name.maxLength` in api/openapi/control-plane.yaml. The
// schema column is unbounded text; the cap is API-edge only.
const NetworkNameMaxLength = 255

// MTU bounds. DefaultMTU is the API-default value used when the
// caller does not specify one on Create — it matches the column
// default introduced in migration 00004. MinMTU is the IPv4 minimum
// per RFC 791 §3.2 (also the OpenAPI lower bound). MaxMTU is the
// practical jumbo-frame ceiling; values above that fail on most
// switches and bridges.
const (
	DefaultMTU = 1500
	MinMTU     = 68
	MaxMTU     = 9216
)

// VLAN tag bounds. The schema CHECK enforces the same range; this
// constant pair lets the API edge return a clean 400 instead of
// surfacing a 23514 (check_violation) at the storage layer.
const (
	MinVLANTag = 1
	MaxVLANTag = 4094
)

// linuxBridgeNameRe matches a syntactically valid Linux bridge
// interface name: 1..15 ASCII characters, leading letter, body in
// [A-Za-z0-9_-]. The 15-char ceiling is IFNAMSIZ-1 (kernel limit).
var linuxBridgeNameRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,14}$`)

// reservedDeviceNameRe matches the Otherix-owned device-name namespace
// (otb<vni> overlay bridge, otvx<vni> VTEP, otwg0 WireGuard). An operator
// type=bridge network must not claim a name in this space or it would shadow
// a derived overlay device.
var reservedDeviceNameRe = regexp.MustCompile(`^(otb|otvx|otwg)`)

// ValidateNetworkType returns nil when t is a recognised
// store.NetworkType value. The set is anchored to the store enum;
// adding a new type means extending both the enum and this branch.
func ValidateNetworkType(t string) error {
	switch store.NetworkType(t) {
	case store.NetworkTypeBridge, store.NetworkTypeOverlay:
		return nil
	default:
		return fmt.Errorf("invalid network type %q (must be one of: bridge, overlay)", t)
	}
}

// ValidateBridgeName returns nil when s is a syntactically valid
// Linux bridge name. The kernel imposes IFNAMSIZ=16 (15 + NUL); the
// regex enforces the practical alphabet.
func ValidateBridgeName(s string) error {
	if s == "" {
		return errors.New("bridge_name is required")
	}
	if !linuxBridgeNameRe.MatchString(s) {
		return fmt.Errorf("invalid bridge_name %q (1..15 chars, [A-Za-z][A-Za-z0-9_-]*)", s)
	}
	if reservedDeviceNameRe.MatchString(s) {
		return fmt.Errorf("invalid bridge_name %q (otb*/otvx*/otwg* are reserved for Otherix-owned devices)", s)
	}
	return nil
}

// ValidateVLANTag returns nil when v is in the IEEE 802.1Q range.
// 0 and 4095 are reserved (untagged and reserved respectively); this
// matches the schema CHECK and the OpenAPI bound.
func ValidateVLANTag(v int) error {
	if v < MinVLANTag || v > MaxVLANTag {
		return fmt.Errorf("invalid vlan_tag %d (must be between %d and %d)", v, MinVLANTag, MaxVLANTag)
	}
	return nil
}

// ValidateMTU returns nil when v is between MinMTU and MaxMTU
// inclusive. Common values: 1500 (Ethernet), 1450 (VPN/overlay
// after IPSec/Geneve overhead), 9000 (jumbo frames).
func ValidateMTU(v int) error {
	if v < MinMTU || v > MaxMTU {
		return fmt.Errorf("invalid mtu %d (must be between %d and %d)", v, MinMTU, MaxMTU)
	}
	return nil
}

// Subnet prefix-length bounds. A managed VM subnet must hold a gateway
// plus at least one usable host, so /31 and /32 (no usable host range)
// are rejected, as is anything wider than /8.
const (
	MinSubnetPrefixLen = 8
	MaxSubnetPrefixLen = 30
)

// ValidateNetworkEgress returns nil when s names a recognised egress
// mode. The empty string is treated as "none". The set is anchored to
// the store.NetworkEgress enum; adding a mode means extending both the
// enum and this branch.
func ValidateNetworkEgress(s string) error {
	switch store.NetworkEgress(s) {
	case "", store.NetworkEgressNone, store.NetworkEgressNAT:
		return nil
	default:
		return fmt.Errorf("invalid egress %q (must be one of: none, nat)", s)
	}
}

// ValidateNetworkInvariants enforces cross-field constraints that no
// single-field validator can express. egress=nat requires a managed
// bridge: NAT masquerading is only meaningful when Otherix owns the
// bridge (managed) and the network is a Layer-2 bridge. An empty egress
// is treated as none and imposes no constraint. The egress value is
// assumed already validated by ValidateNetworkEgress; only nat triggers
// the constraint here.
func ValidateNetworkInvariants(typ store.NetworkType, managed bool, egress store.NetworkEgress) error {
	if egress != store.NetworkEgressNAT {
		return nil
	}
	if typ != store.NetworkTypeBridge || !managed {
		return errors.New("egress=nat requires type=bridge and managed=true")
	}
	return nil
}

// ParseSubnet parses s as an IPv4 CIDR prefix and returns it in
// canonical (masked) form, so host bits in the input are dropped. It
// rejects non-IPv4 prefixes and prefix lengths outside
// [MinSubnetPrefixLen, MaxSubnetPrefixLen].
func ParseSubnet(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid subnet %q: %v", s, err)
	}
	if !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("invalid subnet %q: must be ipv4", s)
	}
	if p.Bits() < MinSubnetPrefixLen || p.Bits() > MaxSubnetPrefixLen {
		return netip.Prefix{}, fmt.Errorf("invalid subnet %q: prefix length must be between %d and %d", s, MinSubnetPrefixLen, MaxSubnetPrefixLen)
	}
	return p.Masked(), nil
}

// GatewayDefault returns the conventional default gateway for subnet:
// the first usable host, i.e. the network address plus one.
func GatewayDefault(subnet netip.Prefix) netip.Addr {
	return subnet.Masked().Addr().Next()
}

// ValidateGatewayInSubnet returns nil when gw is a valid gateway for
// subnet. The checks: gw is IPv4, gw is contained in subnet, gw is not
// the network address, and gw is not the broadcast address. The network
// and broadcast addresses are not assignable to a host.
func ValidateGatewayInSubnet(gw netip.Addr, subnet netip.Prefix) error {
	if !gw.Is4() {
		return fmt.Errorf("invalid gateway %v: must be ipv4", gw)
	}
	if !subnet.Contains(gw) {
		return fmt.Errorf("gateway %v is not within subnet %v", gw, subnet)
	}
	network := subnet.Masked().Addr()
	if gw == network {
		return fmt.Errorf("gateway %v is the network address of %v", gw, subnet)
	}
	if gw == broadcastAddr(subnet) {
		return fmt.Errorf("gateway %v is the broadcast address of %v", gw, subnet)
	}
	return nil
}

// broadcastAddr returns the broadcast address of an IPv4 prefix: the
// network address with all host bits set.
func broadcastAddr(subnet netip.Prefix) netip.Addr {
	network := subnet.Masked()
	b := network.Addr().As4()
	hostBits := 32 - network.Bits()
	for i := 0; i < hostBits; i++ {
		b[3-i/8] |= 1 << (i % 8)
	}
	return netip.AddrFrom4(b)
}
