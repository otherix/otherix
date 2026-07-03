// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package netfabric

import (
	"errors"
	"fmt"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// EnsureVeth idempotently materialises the ingress-gateway veth pair described by
// cfg. It creates the pair when absent (adopt-and-repair re-asserts the host MAC,
// tenant address, sysctls, and peer enslavement when a prior pass left the pair
// half-built), and re-asserting on a healthy pair is harmless. The host-end
// sysctls are load-bearing on a co-located node: rp_filter=2 (loose) lets a guest
// reply that arrives on the veth survive reverse-path filtering even though the
// bridge carries a second connected /24 to the same subnet (the kernel uses
// max(all,iface) and 2 > 1, forcing loose regardless of the global setting);
// arp_ignore=1 / arp_announce=2 keep the veth answering ARP only for the tenant IP
// and announcing the best local source, so the veth and a co-resident bridge
// gateway never answer for each other's address.
func (f *linuxFabric) EnsureVeth(cfg VethConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	host, err := f.ensureVethHostLink(cfg)
	if err != nil {
		return err
	}

	if err := netlink.LinkSetHardwareAddr(host, cfg.MAC); err != nil {
		return fmt.Errorf("netfabric: ensure veth %s: set mac: %v", cfg.HostName, err)
	}
	if cfg.MTU > 0 {
		if err := netlink.LinkSetMTU(host, cfg.MTU); err != nil {
			return fmt.Errorf("netfabric: ensure veth %s: set mtu: %v", cfg.HostName, err)
		}
	}
	a, err := netlink.ParseAddr(cfg.Addr.String())
	if err != nil {
		return fmt.Errorf("netfabric: ensure veth %s: parse addr %s: %v", cfg.HostName, cfg.Addr, err)
	}
	if err := netlink.AddrReplace(host, a); err != nil {
		return fmt.Errorf("netfabric: ensure veth %s: addr: %v", cfg.HostName, err)
	}
	// Sysctls before bringing the link up so a reply can never race a strict
	// reverse-path drop on the first packet.
	for _, s := range []struct{ key, val string }{
		{"rp_filter", "2"},
		{"arp_ignore", "1"},
		{"arp_announce", "2"},
	} {
		if err := writeIfaceSysctl(cfg.HostName, s.key, s.val); err != nil {
			return fmt.Errorf("netfabric: ensure veth %s: %v", cfg.HostName, err)
		}
	}
	if err := netlink.LinkSetUp(host); err != nil {
		return fmt.Errorf("netfabric: ensure veth %s: set up: %v", cfg.HostName, err)
	}
	return f.enslaveVethPeer(cfg)
}

// ensureVethHostLink returns the host-end link, creating the veth pair when
// absent (adopt-and-repair reuses an existing pair). It errors if a non-veth
// link already owns the host name.
func (f *linuxFabric) ensureVethHostLink(cfg VethConfig) (netlink.Link, error) {
	host, err := netlink.LinkByName(cfg.HostName)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if !errors.As(err, &notFound) {
			return nil, fmt.Errorf("netfabric: ensure veth %s: %v", cfg.HostName, err)
		}
		veth := &netlink.Veth{
			LinkAttrs: netlink.LinkAttrs{Name: cfg.HostName, MTU: cfg.MTU},
			PeerName:  cfg.PeerName,
		}
		if err := netlink.LinkAdd(veth); err != nil {
			return nil, fmt.Errorf("netfabric: ensure veth %s: add: %v", cfg.HostName, err)
		}
		host, err = netlink.LinkByName(cfg.HostName)
		if err != nil {
			return nil, fmt.Errorf("netfabric: ensure veth %s: reread: %v", cfg.HostName, err)
		}
		return host, nil
	}
	if _, ok := host.(*netlink.Veth); !ok {
		return nil, fmt.Errorf("netfabric: ensure veth %s: existing link is type %T, not a veth", cfg.HostName, host)
	}
	return host, nil
}

// enslaveVethPeer enslaves the peer end to the overlay bridge and brings it up.
func (f *linuxFabric) enslaveVethPeer(cfg VethConfig) error {
	peer, err := netlink.LinkByName(cfg.PeerName)
	if err != nil {
		return fmt.Errorf("netfabric: ensure veth %s: peer %s: %v", cfg.HostName, cfg.PeerName, err)
	}
	br, err := netlink.LinkByName(cfg.Bridge)
	if err != nil {
		return fmt.Errorf("netfabric: ensure veth %s: bridge %s: %v", cfg.HostName, cfg.Bridge, err)
	}
	if err := netlink.LinkSetMaster(peer, br); err != nil {
		return fmt.Errorf("netfabric: ensure veth %s: enslave peer: %v", cfg.HostName, err)
	}
	if err := netlink.LinkSetUp(peer); err != nil {
		return fmt.Errorf("netfabric: ensure veth %s: peer up: %v", cfg.HostName, err)
	}
	return nil
}

// RemoveVeth deletes the gateway veth pair identified by its host-end name,
// idempotently: deleting one end of a veth pair removes both, so the enslaved
// peer goes with it. An absent link returns nil so repeated teardown is safe.
func (f *linuxFabric) RemoveVeth(host string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	link, err := netlink.LinkByName(host)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("netfabric: remove veth %s: %v", host, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ENODEV) {
			return nil
		}
		return fmt.Errorf("netfabric: remove veth %s: %v", host, err)
	}
	return nil
}
