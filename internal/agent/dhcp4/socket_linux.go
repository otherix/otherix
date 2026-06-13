// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package dhcp4

import (
	"encoding/binary"
	"fmt"
	"net"

	"golang.org/x/sys/unix"

	"github.com/otherix/otherix/internal/agent/netfabric"
)

// afpacketConn is the real Linux AF_PACKET socket bound to one overlay bridge.
// It receives raw Ethernet frames, filters for UDP DHCP requests (dst port 67),
// and frames replies from the overlay gateway (169.254.1.1:67) back to clients.
type afpacketConn struct {
	fd      int
	ifindex int
	srcMAC  net.HardwareAddr // the bridge's own MAC, used as Ethernet source
}

const (
	ethHeaderLen = 14
	ipHeaderLen  = 20
	udpHeaderLen = 8

	dhcpServerPort = 67
	dhcpClientPort = 68
)

// openAFPacket opens a raw AF_PACKET socket bound to the named bridge. The
// socket captures only IPv4 frames (ETH_P_IP); recv further filters to UDP/67.
func openAFPacket(bridge string) (packetConn, error) {
	iface, err := net.InterfaceByName(bridge)
	if err != nil {
		return nil, fmt.Errorf("dhcp4: interface %s: %w", bridge, err)
	}

	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_IP)))
	if err != nil {
		return nil, fmt.Errorf("dhcp4: socket: %w", err)
	}

	sa := &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_IP),
		Ifindex:  iface.Index,
	}
	if err := unix.Bind(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("dhcp4: bind %s: %w", bridge, err)
	}

	// Attach the kernel BPF pre-filter so a guest IPv4 flood is dropped in the
	// kernel instead of waking userspace per frame (audit R2-L4). Fail closed:
	// a socket without its filter would serve the unfiltered firehose.
	if err := attachDHCPFilter(fd); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("dhcp4: attach filter %s: %w", bridge, err)
	}

	return &afpacketConn{fd: fd, ifindex: iface.Index, srcMAC: iface.HardwareAddr}, nil
}

// recv reads frames until it sees a UDP datagram to port 67, returning the UDP
// payload. Non-DHCP frames are skipped.
func (c *afpacketConn) recv() ([]byte, error) {
	buf := make([]byte, 1500)
	for {
		n, _, err := unix.Recvfrom(c.fd, buf, 0)
		if err != nil {
			return nil, err
		}
		payload, ok := parseDHCPFrame(buf[:n])
		if !ok {
			continue
		}
		return payload, nil
	}
}

// send frames payload as Ethernet/IPv4/UDP from the overlay gateway
// (169.254.1.1:67) to dstMAC at the broadcast IP (255.255.255.255:68) and
// writes it on the bound interface.
func (c *afpacketConn) send(payload []byte, dstMAC net.HardwareAddr) error {
	// A DHCP reply is well under an Ethernet MTU; reject anything that would not
	// fit a single IPv4 datagram so the length fields below cannot overflow.
	if len(payload) > 0xffff-ipHeaderLen-udpHeaderLen {
		return fmt.Errorf("dhcp4: reply payload %d bytes too large", len(payload))
	}
	frame := buildFrame(c.srcMAC, dstMAC, payload)

	var ha [8]byte
	copy(ha[:], dstMAC)
	sa := &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_IP),
		Ifindex:  c.ifindex,
		//nolint:gosec // G115: an Ethernet MAC is 6 bytes; Halen never overflows uint8.
		Halen: uint8(len(dstMAC)),
		Addr:  ha,
	}
	return unix.Sendto(c.fd, frame, 0, sa)
}

func (c *afpacketConn) close() error {
	return unix.Close(c.fd)
}

// parseDHCPFrame validates an Ethernet/IPv4/UDP frame and returns the UDP
// payload when it is a DHCP request (UDP dst port 67). It returns ok=false for
// any frame that is not such a request.
func parseDHCPFrame(frame []byte) (payload []byte, ok bool) {
	if len(frame) < ethHeaderLen+ipHeaderLen+udpHeaderLen {
		return nil, false
	}
	// Ethernet: ethertype IPv4.
	if binary.BigEndian.Uint16(frame[12:14]) != unix.ETH_P_IP {
		return nil, false
	}

	ip := frame[ethHeaderLen:]
	// IPv4: version 4, proto UDP. IHL gives the header length in 32-bit words.
	if ip[0]>>4 != 4 {
		return nil, false
	}
	ihl := int(ip[0]&0x0f) * 4
	if ihl < ipHeaderLen || len(ip) < ihl+udpHeaderLen {
		return nil, false
	}
	if ip[9] != unix.IPPROTO_UDP {
		return nil, false
	}

	udp := ip[ihl:]
	if binary.BigEndian.Uint16(udp[2:4]) != dhcpServerPort {
		return nil, false
	}
	udpLen := int(binary.BigEndian.Uint16(udp[4:6]))
	if udpLen < udpHeaderLen || len(udp) < udpLen {
		return nil, false
	}
	return udp[udpHeaderLen:udpLen], true
}

// buildFrame frames payload as Ethernet (srcMAC->dstMAC, IPv4) + IPv4
// (169.254.1.1 -> 255.255.255.255, UDP) + UDP (67 -> 68). The UDP checksum is
// left zero, which is valid for IPv4.
func buildFrame(srcMAC, dstMAC net.HardwareAddr, payload []byte) []byte {
	udpLen := udpHeaderLen + len(payload)
	ipLen := ipHeaderLen + udpLen
	frame := make([]byte, ethHeaderLen+ipLen)

	// Ethernet header.
	copy(frame[0:6], dstMAC)
	copy(frame[6:12], srcMAC)
	binary.BigEndian.PutUint16(frame[12:14], unix.ETH_P_IP)

	// IPv4 header.
	ip := frame[ethHeaderLen:]
	ip[0] = 0x45 // version 4, IHL 5 (20 bytes)
	ip[1] = 0    // DSCP/ECN
	//nolint:gosec // G115: send() rejects payloads that would push ipLen past uint16.
	binary.BigEndian.PutUint16(ip[2:4], uint16(ipLen))
	binary.BigEndian.PutUint16(ip[4:6], 0) // identification
	binary.BigEndian.PutUint16(ip[6:8], 0) // flags/fragment
	ip[8] = 64                             // TTL
	ip[9] = unix.IPPROTO_UDP
	// ip[10:12] checksum left zero for now.
	copy(ip[12:16], netfabric.OverlayGatewayAddr.AsSlice()) // src 169.254.1.1
	ip[16], ip[17], ip[18], ip[19] = 255, 255, 255, 255     // dst broadcast
	binary.BigEndian.PutUint16(ip[10:12], ipChecksum(ip[:ipHeaderLen]))

	// UDP header.
	udp := ip[ipHeaderLen:]
	binary.BigEndian.PutUint16(udp[0:2], dhcpServerPort)
	binary.BigEndian.PutUint16(udp[2:4], dhcpClientPort)
	//nolint:gosec // G115: send() rejects payloads that would push udpLen past uint16.
	binary.BigEndian.PutUint16(udp[4:6], uint16(udpLen))
	binary.BigEndian.PutUint16(udp[6:8], 0) // checksum 0 (valid for IPv4)
	copy(udp[udpHeaderLen:], payload)

	return frame
}

// ipChecksum computes the IPv4 header ones-complement checksum over header,
// which must have its checksum field zeroed.
func ipChecksum(header []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(header); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(header[i : i+2]))
	}
	if len(header)%2 == 1 {
		sum += uint32(header[len(header)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// htons converts a uint16 from host to network byte order.
func htons(v uint16) uint16 {
	return (v<<8)&0xff00 | v>>8
}
