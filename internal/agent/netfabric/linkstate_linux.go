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

	"github.com/vishvananda/netlink"
)

// LinkState returns the observed kernel state of the named link. An absent link
// reports a zero LinkState (Up=false) with a nil error, mirroring BridgeExists /
// VXLANExists, so the overlay gate treats "absent" and "down" alike (both
// pending).
func (f *linuxFabric) LinkState(name string) (LinkState, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return LinkState{}, nil
		}
		return LinkState{}, fmt.Errorf("netfabric: link state %s: %v", name, err)
	}
	attrs := link.Attrs()
	st := LinkState{
		Up:  attrs.Flags&net.FlagUp != 0,
		MTU: attrs.MTU,
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return LinkState{}, fmt.Errorf("netfabric: link state %s: addr list: %v", name, err)
	}
	for _, a := range addrs {
		ip, ok := netip.AddrFromSlice(a.IP)
		if !ok {
			continue
		}
		ones, _ := a.Mask.Size()
		st.Addrs = append(st.Addrs, netip.PrefixFrom(ip.Unmap(), ones))
	}
	return st, nil
}
