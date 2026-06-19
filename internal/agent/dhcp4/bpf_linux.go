// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package dhcp4

import (
	"fmt"

	"golang.org/x/net/bpf"
	"golang.org/x/sys/unix"
)

// dhcpFilterProgram returns the classic-BPF program that accepts only
// IPv4/UDP datagrams to port 67 (the DHCP server port) on an Ethernet-framed
// AF_PACKET socket, dropping everything else in the kernel so a guest IPv4
// flood cannot force a userspace wakeup per frame. It accepts
// the whole frame (return 0xffff) or drops it (return 0). parseDHCPFrame
// remains the userspace second line.
//
// The skip offsets below assume this exact instruction ordering: each
// reject jumps forward to the shared drop instruction at the tail. Reordering
// the program requires recomputing the SkipTrue counts.
func dhcpFilterProgram() []bpf.Instruction {
	const (
		ethTypeOff = 12             // ethertype in the Ethernet header
		ipBase     = ethHeaderLen   // first byte of the IPv4 header (14)
		ipFlagsOff = ipBase + 6     // IPv4 flags+fragment-offset field
		ipProtoOff = ipBase + 9     // IPv4 protocol field
		udpDstOff  = 2              // UDP dst-port offset within the UDP header
		acceptRet  = uint32(0xffff) // capture the whole frame
		dropRet    = uint32(0)      // drop the frame
		fragMask   = uint32(0x1fff) // fragment-offset bits (non-zero => later fragment)
	)
	return []bpf.Instruction{
		// [0] ethertype == IPv4 ? else drop. Drop is at index 9, so from
		// index 1 the true branch skips instructions 2..8 (7 instructions).
		bpf.LoadAbsolute{Off: ethTypeOff, Size: 2},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: uint32(unix.ETH_P_IP), SkipTrue: 7},
		// [2] ip proto == UDP ? else drop (index 9): skip 2..8 from index 3 => 5.
		bpf.LoadAbsolute{Off: ipProtoOff, Size: 1},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: uint32(unix.IPPROTO_UDP), SkipTrue: 5},
		// [4] not a later fragment: (flags+frag & 0x1fff) == 0 ? A set bit
		// means a later fragment (ports live only in the first fragment), so
		// drop (index 9): skip 6..8 from index 5 => 3.
		bpf.LoadAbsolute{Off: ipFlagsOff, Size: 2},
		bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: fragMask, SkipTrue: 3},
		// [6] X = IPv4 header length (IHL*4) read from the low nibble at ipBase.
		bpf.LoadMemShift{Off: ipBase},
		// [7] UDP dst port at X + ipBase + udpDstOff.
		bpf.LoadIndirect{Off: ipBase + udpDstOff, Size: 2},
		// [8] dst port == 67 ? accept (index 10): skip index 9 => 1; else fall
		// through to drop at index 9.
		bpf.JumpIf{Cond: bpf.JumpEqual, Val: uint32(dhcpServerPort), SkipTrue: 1},
		// [9] drop.
		bpf.RetConstant{Val: dropRet},
		// [10] accept.
		bpf.RetConstant{Val: acceptRet},
	}
}

// attachDHCPFilter installs dhcpFilterProgram on fd via SO_ATTACH_FILTER. A
// socket that cannot install its filter is a hard error: the caller fails
// closed rather than serving the unfiltered firehose.
func attachDHCPFilter(fd int) error {
	insts, err := bpf.Assemble(dhcpFilterProgram())
	if err != nil {
		return fmt.Errorf("dhcp4: assemble bpf: %w", err)
	}
	raw := make([]unix.SockFilter, len(insts))
	for i, in := range insts {
		raw[i] = unix.SockFilter{Code: in.Op, Jt: in.Jt, Jf: in.Jf, K: in.K}
	}
	//nolint:gosec // G115: a cBPF program is a handful of instructions; len never overflows uint16.
	prog := &unix.SockFprog{Len: uint16(len(raw)), Filter: &raw[0]}
	return unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, prog)
}
