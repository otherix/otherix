// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package netfabric

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/vishvananda/netlink"
)

// CreateTap creates a persistent TAP-mode tuntap device with the given
// name, sets its MTU when mtu is positive, and brings it up. It is
// idempotent: a second call against an existing tap only reapplies MTU
// and the up state. The device is created without packet information
// (TUNTAP_NO_PI) so QEMU sees raw Ethernet frames.
func (f *linuxFabric) CreateTap(name string, mtu int) error {
	// Guard the MTU at the agent edge: the kernel rejects out-of-range
	// values, and we never want to hand it a bogus one even though the CP
	// edge already clamps. 0 means "leave the device default".
	if mtu < 0 || mtu > 65535 {
		return fmt.Errorf("netfabric: create tap %s: mtu %d out of range [0,65535]", name, mtu)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

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
	} else if _, ok := link.(*netlink.Tuntap); !ok {
		// A link of this name already exists but is not a tap. Reusing it
		// would let a name collision silently adopt a leftover bridge or
		// another VM's device, and a later DeleteTap would destroy it.
		return fmt.Errorf("netfabric: create tap %s: existing link is type %T, not a tap", name, link)
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
	f.mu.Lock()
	defer f.mu.Unlock()

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
	f.mu.Lock()
	defer f.mu.Unlock()

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

// ListTaps returns the names of every Otherix-managed tap device on the
// host: each tuntap link whose name carries the tapPrefix that
// NIC.TapName uses. Non-tap links and taps named outside that prefix are
// ignored, so an operator's own tuntap devices are never reported. The
// names are returned sorted.
func (f *linuxFabric) ListTaps() ([]string, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("netfabric: list taps: %v", err)
	}
	var names []string
	for _, link := range links {
		if _, ok := link.(*netlink.Tuntap); !ok {
			continue
		}
		name := link.Attrs().Name
		if !strings.HasPrefix(name, tapPrefix) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
