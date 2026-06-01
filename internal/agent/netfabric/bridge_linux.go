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

// EnsureBridge creates the named Linux bridge if it does not exist, sets
// its MTU when mtu is positive, and brings it up. It is idempotent: a
// second call against an existing bridge only reapplies MTU and the up
// state.
func (f *linuxFabric) EnsureBridge(name string, mtu int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("netfabric: ensure bridge %s: %v", name, err)
		}
		br := &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name, MTU: mtu}}
		if err := netlink.LinkAdd(br); err != nil {
			return fmt.Errorf("netfabric: ensure bridge %s: %v", name, err)
		}
		link = br
	}
	if mtu > 0 {
		if err := netlink.LinkSetMTU(link, mtu); err != nil {
			return fmt.Errorf("netfabric: ensure bridge %s: set mtu: %v", name, err)
		}
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("netfabric: ensure bridge %s: set up: %v", name, err)
	}
	return nil
}

// RemoveBridge deletes the named bridge. It returns nil if the bridge is
// already absent.
func (f *linuxFabric) RemoveBridge(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("netfabric: remove bridge %s: %v", name, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("netfabric: remove bridge %s: %v", name, err)
	}
	return nil
}

// BridgeExists reports whether a bridge with the given name exists. A
// link of a non-bridge type with the same name reports false.
func (f *linuxFabric) BridgeExists(name string) (bool, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, fmt.Errorf("netfabric: bridge exists %s: %v", name, err)
	}
	if _, ok := link.(*netlink.Bridge); !ok {
		return false, nil
	}
	return true, nil
}
