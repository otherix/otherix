// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package netfabric

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/netip"
)

// OverlayGatewayAddr is the link-local anycast address Otherix puts on every
// overlay bridge (otvb<vni>) as the VM-facing default gateway and DNS server.
// It is identical on every node, so a VM keeps the same next-hop across live
// migration; the local node always answers. RFC 3927 link-local space, so it
// never collides with a tenant overlay subnet.
var OverlayGatewayAddr = netip.MustParseAddr("169.254.1.1")

// OverlayDNSPort is the UDP port the per-node DNS forwarder listens on at
// OverlayGatewayAddr. The gateway address doubles as the resolver.
const OverlayDNSPort = 53

// GatewayMAC returns the deterministic anycast gateway MAC for an overlay VNI.
// First octet 0x02 marks it locally-administered + unicast; the VNI (24-bit)
// fills the low three octets, so the MAC is uniform across nodes for one VNI
// (anycast works, no flooding across the VXLAN) yet distinct per overlay on a
// host (no duplicate MAC on the same machine).
func GatewayMAC(vni uint32) net.HardwareAddr {
	//nolint:gosec // G115: VNI is a 24-bit VXLAN id; truncating each shifted octet to a byte is the intended encoding.
	return net.HardwareAddr{0x02, 0x00, 0x00, byte(vni >> 16), byte(vni >> 8), byte(vni)}
}

// GatewayMACFromID returns the deterministic anycast gateway MAC for a managed
// bridge keyed on its network id (bridges have no VNI). First octet 0x02 marks
// it locally-administered + unicast; the remaining octets are a stable hash of
// id, so the MAC is uniform across nodes for one network (anycast works) yet
// distinct per network on a host. The 0x02,0xBB prefix keeps it out of the
// overlay VNI MAC space (0x02,0x00,0x00,<vni>).
func GatewayMACFromID(id string) net.HardwareAddr {
	sum := sha256.Sum256([]byte(id))
	return net.HardwareAddr{0x02, 0xBB, sum[0], sum[1], sum[2], sum[3]}
}

// GatewayVethHostName returns the deterministic root-namespace host-end name of
// an ingress gateway's veth pair for an overlay VNI: otvg<vni>. The host end
// carries the membership's tenant IP + unicast MAC and originates the gateway's
// guest dials. Deriving it from the VNI (like otvb<vni> / otvx<vni>) lets teardown
// and adopt-and-repair find the pair with no stored state.
func GatewayVethHostName(vni uint32) string { return fmt.Sprintf("otvg%d", vni) }

// GatewayVethPeerName returns the deterministic peer-end name of an ingress
// gateway's veth pair for an overlay VNI: otvgp<vni>. The peer end is enslaved to
// otvb<vni> as an ordinary bridge port. It is referenced only at creation;
// RemoveVeth deletes the pair by its host-end name (deleting one end removes both).
func GatewayVethPeerName(vni uint32) string { return fmt.Sprintf("otvgp%d", vni) }
