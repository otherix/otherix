// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package netfabric

import (
	"errors"
	"fmt"

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
	if cfg.Name == "" {
		return fmt.Errorf("netfabric: ensure wireguard: empty interface name")
	}
	if cfg.MTU < 0 || cfg.MTU > 65535 {
		return fmt.Errorf("netfabric: ensure wireguard %s: mtu %d out of range [0,65535]", cfg.Name, cfg.MTU)
	}
	if !cfg.Address.IsValid() {
		return fmt.Errorf("netfabric: ensure wireguard %s: invalid address", cfg.Name)
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
	defer c.Close()
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
