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
	"sort"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// EnsureWireGuard creates the named WireGuard link if absent, configures its
// private key and listen port via wgctrl, assigns Address, sets MTU and brings
// it up. It is idempotent: a second call reapplies the same configuration. An
// existing same-name link that is not a WireGuard device is rejected rather
// than adopted, so a name collision never lets RemoveWireGuard later destroy a
// foreign device (mirrors the EnsureVXLAN / CreateTap type-check discipline).
func (f *linuxFabric) EnsureWireGuard(cfg WGConfig) error {
	if err := validateWGConfig(cfg); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	link, err := netlink.LinkByName(cfg.Name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("netfabric: ensure wireguard %s: %v", cfg.Name, err)
		}
		wg := &netlink.Wireguard{LinkAttrs: netlink.LinkAttrs{Name: cfg.Name, MTU: cfg.MTU}}
		if err := netlink.LinkAdd(wg); err != nil {
			return fmt.Errorf("netfabric: ensure wireguard %s: %v", cfg.Name, err)
		}
		link = wg
	} else if _, ok := link.(*netlink.Wireguard); !ok {
		return fmt.Errorf("netfabric: ensure wireguard %s: existing link is type %T, not wireguard", cfg.Name, link)
	}

	c, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("netfabric: ensure wireguard %s: wgctrl: %v", cfg.Name, err)
	}
	defer func() { _ = c.Close() }()
	key := cfg.PrivateKey
	port := cfg.ListenPort
	if err := c.ConfigureDevice(cfg.Name, wgtypes.Config{PrivateKey: &key, ListenPort: &port}); err != nil {
		return fmt.Errorf("netfabric: ensure wireguard %s: configure: %v", cfg.Name, err)
	}

	addr, err := netlink.ParseAddr(cfg.Address.String())
	if err != nil {
		return fmt.Errorf("netfabric: ensure wireguard %s: parse addr %q: %v", cfg.Name, cfg.Address, err)
	}
	if err := netlink.AddrReplace(link, addr); err != nil {
		return fmt.Errorf("netfabric: ensure wireguard %s: assign addr: %v", cfg.Name, err)
	}
	if cfg.MTU > 0 {
		if err := netlink.LinkSetMTU(link, cfg.MTU); err != nil {
			return fmt.Errorf("netfabric: ensure wireguard %s: set mtu: %v", cfg.Name, err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("netfabric: ensure wireguard %s: set up: %v", cfg.Name, err)
	}
	return nil
}

// validateWGConfig rejects an out-of-range or incomplete WGConfig before any
// netlink mutation, so EnsureWireGuard fails closed on bad input.
func validateWGConfig(cfg WGConfig) error {
	if cfg.Name == "" {
		return fmt.Errorf("netfabric: ensure wireguard: empty interface name")
	}
	if cfg.MTU < 0 || cfg.MTU > 65535 {
		return fmt.Errorf("netfabric: ensure wireguard %s: mtu %d out of range [0,65535]", cfg.Name, cfg.MTU)
	}
	if cfg.ListenPort < 0 || cfg.ListenPort > 65535 {
		return fmt.Errorf("netfabric: ensure wireguard %s: listen port %d out of range [0,65535]", cfg.Name, cfg.ListenPort)
	}
	if !cfg.Address.IsValid() {
		return fmt.Errorf("netfabric: ensure wireguard %s: invalid address", cfg.Name)
	}
	return nil
}

// RemoveWireGuard deletes the named WireGuard link. It returns nil if the link
// is already absent.
func (f *linuxFabric) RemoveWireGuard(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("netfabric: remove wireguard %s: %v", name, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("netfabric: remove wireguard %s: %v", name, err)
	}
	return nil
}

// WireGuardExists reports whether a WireGuard link of the given name exists. A
// link of the same name but a non-wireguard type reports false.
func (f *linuxFabric) WireGuardExists(name string) (bool, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("netfabric: wireguard exists %s: %v", name, err)
	}
	if _, ok := link.(*netlink.Wireguard); !ok {
		return false, nil
	}
	return true, nil
}

// prefixToIPNet converts a netip.Prefix to the net.IPNet wgctrl expects for an
// allowed IP. Unmap guards against an IPv4-in-IPv6 address widening the mask to
// 128 bits.
func prefixToIPNet(p netip.Prefix) net.IPNet {
	a := p.Addr().Unmap()
	return net.IPNet{IP: a.AsSlice(), Mask: net.CIDRMask(p.Bits(), a.BitLen())}
}

// ipNetToPrefix converts a net.IPNet read back from wgctrl to a netip.Prefix.
func ipNetToPrefix(n net.IPNet) (netip.Prefix, bool) {
	a, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, bits := n.Mask.Size()
	if bits == 0 {
		// A non-contiguous mask reports (0, 0); reject it rather than
		// silently fabricate a /0 so the bool return honestly signals failure.
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a.Unmap(), ones), true
}

// SetWireGuardPeers atomically replaces the full peer set on the named
// interface via wgctrl ReplacePeers. PersistentKeepalive and AllowedIPs are
// applied per peer.
func (f *linuxFabric) SetWireGuardPeers(name string, peers []WGPeer) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("netfabric: set wireguard peers %s: wgctrl: %v", name, err)
	}
	defer func() { _ = c.Close() }()

	cfgs := make([]wgtypes.PeerConfig, 0, len(peers))
	for _, p := range peers {
		allowed := make([]net.IPNet, 0, len(p.AllowedIPs))
		for _, a := range p.AllowedIPs {
			allowed = append(allowed, prefixToIPNet(a))
		}
		pc := wgtypes.PeerConfig{
			PublicKey:         p.PublicKey,
			Endpoint:          p.Endpoint,
			ReplaceAllowedIPs: true,
			AllowedIPs:        allowed,
		}
		if p.PersistentKeepalive > 0 {
			ka := p.PersistentKeepalive
			pc.PersistentKeepaliveInterval = &ka
		}
		cfgs = append(cfgs, pc)
	}
	if err := c.ConfigureDevice(name, wgtypes.Config{ReplacePeers: true, Peers: cfgs}); err != nil {
		return fmt.Errorf("netfabric: set wireguard peers %s: configure: %v", name, err)
	}
	return nil
}

// WireGuardPeers returns the configured peers on the named interface, sorted by
// public key for deterministic observed-state reporting.
func (f *linuxFabric) WireGuardPeers(name string) ([]WGPeer, error) {
	c, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("netfabric: wireguard peers %s: wgctrl: %v", name, err)
	}
	defer func() { _ = c.Close() }()
	dev, err := c.Device(name)
	if err != nil {
		return nil, fmt.Errorf("netfabric: wireguard peers %s: %v", name, err)
	}
	out := make([]WGPeer, 0, len(dev.Peers))
	for _, p := range dev.Peers {
		allowed := make([]netip.Prefix, 0, len(p.AllowedIPs))
		for _, a := range p.AllowedIPs {
			if pfx, ok := ipNetToPrefix(a); ok {
				allowed = append(allowed, pfx)
			}
		}
		out = append(out, WGPeer{
			PublicKey:           p.PublicKey,
			Endpoint:            p.Endpoint,
			AllowedIPs:          allowed,
			PersistentKeepalive: p.PersistentKeepaliveInterval,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PublicKey.String() < out[j].PublicKey.String() })
	return out, nil
}

// WireGuardPeerHandshakes returns the observed per-peer handshake state on the
// named interface, sorted by public key for deterministic reporting. Lock-free
// (a read, no mutator).
func (f *linuxFabric) WireGuardPeerHandshakes(name string) ([]WGPeerHandshake, error) {
	c, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("netfabric: wireguard peer handshakes %s: wgctrl: %v", name, err)
	}
	defer func() { _ = c.Close() }()
	dev, err := c.Device(name)
	if err != nil {
		return nil, fmt.Errorf("netfabric: wireguard peer handshakes %s: %v", name, err)
	}
	out := make([]WGPeerHandshake, 0, len(dev.Peers))
	for _, p := range dev.Peers {
		out = append(out, WGPeerHandshake{
			PublicKey:     p.PublicKey,
			LastHandshake: p.LastHandshakeTime,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PublicKey.String() < out[j].PublicKey.String() })
	return out, nil
}
