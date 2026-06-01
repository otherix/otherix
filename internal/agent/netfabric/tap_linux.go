// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package netfabric

import (
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"
)

// CreateTap creates a persistent TAP-mode tuntap device with the given
// name, sets its MTU when mtu is positive, and brings it up. It is
// idempotent: a second call against an existing tap only reapplies MTU
// and the up state. The device is created without packet information
// (TUNTAP_NO_PI) so QEMU sees raw Ethernet frames.
func (f *linuxFabric) CreateTap(name string, mtu int) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("netfabric: create tap %s: %v", name, err)
		}
		tap := &netlink.Tuntap{
			LinkAttrs: netlink.LinkAttrs{Name: name, MTU: mtu},
			Mode:      netlink.TUNTAP_MODE_TAP,
			Flags:     netlink.TUNTAP_NO_PI,
		}
		if err := netlink.LinkAdd(tap); err != nil {
			return fmt.Errorf("netfabric: create tap %s: %v", name, err)
		}
		link = tap
	}
	if mtu > 0 {
		if err := netlink.LinkSetMTU(link, mtu); err != nil {
			return fmt.Errorf("netfabric: create tap %s: set mtu: %v", name, err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("netfabric: create tap %s: set up: %v", name, err)
	}
	return nil
}

// AttachTap enslaves the named tap device to the named bridge. It
// returns an error if either link is absent.
func (f *linuxFabric) AttachTap(tap, bridge string) error {
	tapLink, err := netlink.LinkByName(tap)
	if err != nil {
		return fmt.Errorf("netfabric: attach tap %s to %s: tap: %v", tap, bridge, err)
	}
	brLink, err := netlink.LinkByName(bridge)
	if err != nil {
		return fmt.Errorf("netfabric: attach tap %s to %s: bridge: %v", tap, bridge, err)
	}
	if err := netlink.LinkSetMaster(tapLink, brLink); err != nil {
		return fmt.Errorf("netfabric: attach tap %s to %s: %v", tap, bridge, err)
	}
	return nil
}

// DeleteTap removes the named tap device. It returns nil if the device
// is already absent.
func (f *linuxFabric) DeleteTap(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("netfabric: delete tap %s: %v", name, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("netfabric: delete tap %s: %v", name, err)
	}
	return nil
}
