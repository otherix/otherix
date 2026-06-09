// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package netfabric

import (
	"net"
	"net/netip"
)

// OverlayGatewayAddr is the link-local anycast address Otherix puts on every
// overlay bridge (otb<vni>) as the VM-facing default gateway and DNS server.
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
	return net.HardwareAddr{0x02, 0x00, 0x00, byte(vni >> 16), byte(vni >> 8), byte(vni)}
}
