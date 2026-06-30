// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package netfabric

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
)

// EnsureUnicastGateway pins mac as the bridge link's hardware address and
// assigns addr/32 to it, idempotently. It is the unicast counterpart of
// EnsureAnycastGateway: an ingress gateway claims a distinct per-membership
// unicast MAC drawn from the network's address space (never the shared anycast
// MAC, which is identical on every node and can never be a unicast FDB target)
// so the host kernel originates and answers at the tenant addr and return
// traffic to the MAC advertised in the overlay FDB is delivered to this bridge.
// Setting the bridge hardware address explicitly also stops the kernel
// auto-inheriting the lowest enslaved-port MAC; a re-assert each reconcile pass
// is harmless. A gateway runs this in place of the anycast services plane, so
// the two never re-assert opposite hardware addresses on the same bridge.
func (f *linuxFabric) EnsureUnicastGateway(bridge string, addr netip.Addr, mac net.HardwareAddr) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	link, err := netlink.LinkByName(bridge)
	if err != nil {
		return fmt.Errorf("netfabric: ensure unicast gateway on %s: %v", bridge, err)
	}
	if err := netlink.LinkSetHardwareAddr(link, mac); err != nil {
		return fmt.Errorf("netfabric: ensure unicast gateway on %s: set mac: %v", bridge, err)
	}
	p := netip.PrefixFrom(addr, addr.BitLen()) // /32 for IPv4
	a, err := netlink.ParseAddr(p.String())
	if err != nil {
		return fmt.Errorf("netfabric: ensure unicast gateway on %s: parse %s: %v", bridge, p, err)
	}
	if err := netlink.AddrReplace(link, a); err != nil {
		return fmt.Errorf("netfabric: ensure unicast gateway on %s: %v", bridge, err)
	}
	return nil
}
