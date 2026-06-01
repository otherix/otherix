// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package netfabric

import (
	"errors"
	"fmt"
	"net"

	"github.com/vishvananda/netlink"
)

// vxlanName returns the deterministic device name for a VNI: "otvx"
// followed by the decimal VNI (e.g. otvx1000), per the overlay naming
// convention.
func vxlanName(vni uint32) string {
	return fmt.Sprintf("otvx%d", vni)
}

// EnsureVXLAN creates the otvx<vni> VXLAN VTEP if absent, sets its MTU and
// brings it up. It is idempotent: a second call against an existing VTEP of
// the same name only reapplies MTU and the up state. Learning is off (the
// FDB is controller-authoritative), and the VTEP is bound to cfg.Local
// (loopback for the single-agent N1b scaffold) with no multicast group, so
// remotes are added exclusively through FDBAppend.
func (f *linuxFabric) EnsureVXLAN(cfg VXLANConfig) error {
	if cfg.MTU < 0 || cfg.MTU > 65535 {
		return fmt.Errorf("netfabric: ensure vxlan %d: mtu %d out of range [0,65535]", cfg.VNI, cfg.MTU)
	}
	if !cfg.Local.IsValid() {
		return fmt.Errorf("netfabric: ensure vxlan %d: invalid local addr", cfg.VNI)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	name := vxlanName(cfg.VNI)
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("netfabric: ensure vxlan %s: %v", name, err)
		}
		vx := &netlink.Vxlan{
			LinkAttrs: netlink.LinkAttrs{Name: name, MTU: cfg.MTU},
			VxlanId:   int(cfg.VNI),
			SrcAddr:   net.IP(cfg.Local.Unmap().AsSlice()),
			Port:      int(cfg.Port),
			Learning:  false,
		}
		if err := netlink.LinkAdd(vx); err != nil {
			return fmt.Errorf("netfabric: ensure vxlan %s: %v", name, err)
		}
		link = vx
	} else if _, ok := link.(*netlink.Vxlan); !ok {
		// A link of this name exists but is not a VXLAN. Reusing it would let
		// a name collision silently adopt a foreign device that RemoveVXLAN
		// would later destroy (mirrors CreateTap's type-check, review I3).
		return fmt.Errorf("netfabric: ensure vxlan %s: existing link is type %T, not a vxlan", name, link)
	}
	if cfg.MTU > 0 {
		if err := netlink.LinkSetMTU(link, cfg.MTU); err != nil {
			return fmt.Errorf("netfabric: ensure vxlan %s: set mtu: %v", name, err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("netfabric: ensure vxlan %s: set up: %v", name, err)
	}
	return nil
}

// RemoveVXLAN deletes the otvx<vni> VTEP. It returns nil if the device is
// already absent.
func (f *linuxFabric) RemoveVXLAN(vni uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	name := vxlanName(vni)
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("netfabric: remove vxlan %s: %v", name, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("netfabric: remove vxlan %s: %v", name, err)
	}
	return nil
}

// VXLANExists reports whether the otvx<vni> VTEP exists. A link of a
// non-vxlan type with the same name reports false.
func (f *linuxFabric) VXLANExists(vni uint32) (bool, error) {
	name := vxlanName(vni)
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("netfabric: vxlan exists %s: %v", name, err)
	}
	if _, ok := link.(*netlink.Vxlan); !ok {
		return false, nil
	}
	return true, nil
}
