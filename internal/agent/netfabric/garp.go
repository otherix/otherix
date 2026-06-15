// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package netfabric

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
)

// buildGratuitousARP builds a complete Ethernet frame carrying a gratuitous ARP
// reply: opcode REPLY, sender hardware/protocol = target hardware/protocol = the
// VM's MAC/IPv4, broadcast destination. It announces "this MAC owns this IP" to
// the L2 segment so a learning bridge or physical switch relearns the port and
// neighbor ARP caches refresh. ip must be IPv4.
func buildGratuitousARP(mac net.HardwareAddr, ip netip.Addr) ([]byte, error) {
	if len(mac) != 6 {
		return nil, fmt.Errorf("netfabric: garp: mac %q is not a 6-octet address", mac)
	}
	if !ip.Is4() {
		return nil, fmt.Errorf("netfabric: garp: ip %s is not IPv4", ip)
	}
	v4 := ip.As4()
	f := make([]byte, 42)
	copy(f[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	copy(f[6:12], mac)
	binary.BigEndian.PutUint16(f[12:14], 0x0806)
	binary.BigEndian.PutUint16(f[14:16], 1)
	binary.BigEndian.PutUint16(f[16:18], 0x0800)
	f[18] = 6
	f[19] = 4
	binary.BigEndian.PutUint16(f[20:22], 2)
	copy(f[22:28], mac)
	copy(f[28:32], v4[:])
	copy(f[32:38], mac)
	copy(f[38:42], v4[:])
	return f, nil
}

func htons(v uint16) uint16 { return (v<<8)&0xff00 | v>>8 }
