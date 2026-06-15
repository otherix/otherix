// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux

package netfabric

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// SendGARP broadcasts a gratuitous ARP for mac/ip on the named bridge so the
// fleet relearns the VM's location after a live migration cutover. Best-effort
// at the call site: a send failure degrades to the FDB/heartbeat convergence,
// never aborts the resume.
func (f *linuxFabric) SendGARP(bridge string, mac string, ip netip.Addr) error {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return fmt.Errorf("netfabric: garp on %s: parse mac %q: %v", bridge, mac, err)
	}
	frame, err := buildGratuitousARP(hw, ip)
	if err != nil {
		return err
	}
	link, err := netlink.LinkByName(bridge)
	if err != nil {
		return fmt.Errorf("netfabric: garp on %s: %v", bridge, err)
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, 0)
	if err != nil {
		return fmt.Errorf("netfabric: garp on %s: socket: %v", bridge, err)
	}
	defer func() { _ = unix.Close(fd) }()
	addr := &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_ARP),
		Ifindex:  link.Attrs().Index,
		Halen:    6,
	}
	copy(addr.Addr[:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	if err := unix.Sendto(fd, frame, 0, addr); err != nil {
		return fmt.Errorf("netfabric: garp on %s: sendto: %v", bridge, err)
	}
	return nil
}

// htons converts a uint16 from host to network byte order for the AF_PACKET
// socket address protocol field.
func htons(v uint16) uint16 { return (v<<8)&0xff00 | v>>8 }
