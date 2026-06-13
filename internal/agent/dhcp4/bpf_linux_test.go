// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package dhcp4

import (
	"encoding/binary"
	"testing"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

// makeFrame lays out a minimal Ethernet(14) + IPv4(20, IHL=5) + UDP(8) frame
// with the given ethertype, IP protocol, UDP destination port, and IPv4
// fragment-offset field. It mirrors the byte offsets the filter program and
// parseDHCPFrame use: ethertype at [12:14], flags+fragment at [20:22], IP
// proto at [23], UDP dst port at [36:38].
func makeFrame(ethertype, ipProto uint16, dstPort uint16, fragOffset uint16) []byte {
	frame := make([]byte, ethHeaderLen+ipHeaderLen+udpHeaderLen)
	binary.BigEndian.PutUint16(frame[12:14], ethertype)

	ip := frame[ethHeaderLen:]
	ip[0] = 0x45 // version 4, IHL 5 (20 bytes)
	binary.BigEndian.PutUint16(ip[6:8], fragOffset)
	ip[9] = byte(ipProto)

	udp := ip[ipHeaderLen:]
	binary.BigEndian.PutUint16(udp[2:4], dstPort)
	return frame
}

// TestDHCPFilterAcceptsOnlyUDP67 proves the cBPF program accepts only
// IPv4/UDP datagrams to port 67 and drops everything else, running the
// program in the pure-Go bpf.VM (no privileges, runs on any host).
func TestDHCPFilterAcceptsOnlyUDP67(t *testing.T) {
	vm, err := bpf.NewVM(dhcpFilterProgram())
	if err != nil {
		t.Fatalf("bpf.NewVM: %v", err)
	}
	tests := []struct {
		name      string
		ethertype uint16
		ipProto   uint16
		dstPort   uint16
		fragOff   uint16
		accept    bool
	}{
		{"udp/67 first fragment accepted", unix.ETH_P_IP, unix.IPPROTO_UDP, 67, 0, true},
		{"udp/53 dropped", unix.ETH_P_IP, unix.IPPROTO_UDP, 53, 0, false},
		{"tcp/67 dropped", unix.ETH_P_IP, unix.IPPROTO_TCP, 67, 0, false},
		{"non-ipv4 dropped", unix.ETH_P_ARP, unix.IPPROTO_UDP, 67, 0, false},
		{"udp/67 later fragment dropped", unix.ETH_P_IP, unix.IPPROTO_UDP, 67, 0x0020, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frame := makeFrame(tc.ethertype, tc.ipProto, tc.dstPort, tc.fragOff)
			n, err := vm.Run(frame)
			if err != nil {
				t.Fatalf("vm.Run: %v", err)
			}
			if got := n != 0; got != tc.accept {
				t.Errorf("vm.Run(%s) accepted = %v (ret %d), want %v", tc.name, got, n, tc.accept)
			}
		})
	}
}
