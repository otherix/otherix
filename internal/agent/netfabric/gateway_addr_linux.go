// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package netfabric

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// EnsureUnicastGateway pins mac as the bridge link's hardware address and
// assigns addr to it, idempotently. addr carries the overlay subnet's prefix
// length (e.g. /24) so the kernel installs an on-link route for the whole
// overlay subnet via the bridge: the gateway then reaches guest VMs over the
// overlay rather than leaking their traffic out the host default route (a bare
// /32 host address gives no route to the subnet). It is the unicast counterpart
// of EnsureAnycastGateway: an ingress gateway claims a distinct per-membership
// unicast MAC drawn from the network's address space (never the shared anycast
// MAC, which is identical on every node and can never be a unicast FDB target)
// so the host kernel originates and answers at the tenant addr and return
// traffic to the MAC advertised in the overlay FDB is delivered to this bridge.
// Setting the bridge hardware address explicitly also stops the kernel
// auto-inheriting the lowest enslaved-port MAC; a re-assert each reconcile pass
// is harmless. A gateway runs this in place of the anycast services plane, so
// the two never re-assert opposite hardware addresses on the same bridge.
func (f *linuxFabric) EnsureUnicastGateway(bridge string, addr netip.Prefix, mac net.HardwareAddr) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	link, err := netlink.LinkByName(bridge)
	if err != nil {
		return fmt.Errorf("netfabric: ensure unicast gateway on %s: %v", bridge, err)
	}
	if err := netlink.LinkSetHardwareAddr(link, mac); err != nil {
		return fmt.Errorf("netfabric: ensure unicast gateway on %s: set mac: %v", bridge, err)
	}
	a, err := netlink.ParseAddr(addr.String())
	if err != nil {
		return fmt.Errorf("netfabric: ensure unicast gateway on %s: parse %s: %v", bridge, addr, err)
	}
	if err := netlink.AddrReplace(link, a); err != nil {
		return fmt.Errorf("netfabric: ensure unicast gateway on %s: %v", bridge, err)
	}

	// Claim the bridge exclusively for unicast: strip any leftover shared anycast
	// gateway address. A node repurposed into a gateway can carry the anycast
	// address from its earlier hypervisor-agent life; left in place, the kernel
	// may pick it as the source for guest-subnet traffic, and the guest's reply to
	// the anycast address is answered by its own local node rather than returning
	// to this gateway, so every dial to a guest times out.
	anycast, err := netlink.ParseAddr(netip.PrefixFrom(OverlayGatewayAddr, OverlayGatewayAddr.BitLen()).String())
	if err != nil {
		return fmt.Errorf("netfabric: ensure unicast gateway on %s: parse anycast: %v", bridge, err)
	}
	if err := netlink.AddrDel(link, anycast); err != nil &&
		!errors.Is(err, unix.EADDRNOTAVAIL) && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("netfabric: ensure unicast gateway on %s: remove anycast: %v", bridge, err)
	}
	return nil
}
