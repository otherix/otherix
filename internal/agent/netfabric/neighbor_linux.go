// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux

package netfabric

import (
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// neighborResolvedStates is the set of kernel NUD states in which a neighbor
// entry carries a trustworthy link-layer address. FAILED / INCOMPLETE / NONE
// are excluded: they either have no MAC or an unconfirmed one, so treating them
// as resolved would let the anti-SSRF MAC binding pass on stale or absent data.
const neighborResolvedStates = netlink.NUD_REACHABLE | netlink.NUD_STALE |
	netlink.NUD_DELAY | netlink.NUD_PROBE | netlink.NUD_PERMANENT

const (
	// neighborProbeTimeout bounds the best-effort probe datagram used to provoke
	// kernel ARP/ND resolution when the first table read finds nothing.
	neighborProbeTimeout = 200 * time.Millisecond
	// neighborProbeWait is how long to wait after the probe before re-reading the
	// neighbor table, giving the kernel time to record the reply.
	neighborProbeWait = 150 * time.Millisecond
	// neighborProbePort is an arbitrary high discard port the probe datagram
	// targets. The datagram only needs to be sent for the kernel to resolve the
	// on-link next hop; nothing has to be listening.
	neighborProbePort = "9"
)

// NeighborMAC resolves ip to its link-layer address on the named bridge.
func (f *linuxFabric) NeighborMAC(bridge string, ip netip.Addr) (net.HardwareAddr, bool, error) {
	if !ip.IsValid() {
		return nil, false, fmt.Errorf("netfabric: neighbor lookup on %s: invalid ip", bridge)
	}
	link, err := netlink.LinkByName(bridge)
	if err != nil {
		return nil, false, fmt.Errorf("netfabric: neighbor lookup on %s: %v", bridge, err)
	}
	idx := link.Attrs().Index

	mac, ok, err := neighborLookup(idx, ip)
	if err != nil || ok {
		return mac, ok, err
	}

	// The neighbor is not yet resolved. Provoke kernel ARP/ND by sending one
	// datagram toward the address, wait briefly, and re-read once. Best-effort: a
	// failed probe leaves the lookup unresolved and the caller refuses, so this
	// never weakens the binding.
	probeNeighbor(ip, bridge)
	return neighborLookup(idx, ip)
}

// neighborLookup returns the resolved MAC for ip among the kernel neighbor
// entries on the link at linkIndex. ok is false when no entry for ip is in a
// resolved state.
func neighborLookup(linkIndex int, ip netip.Addr) (net.HardwareAddr, bool, error) {
	family := unix.AF_INET
	if ip.Is6() {
		family = unix.AF_INET6
	}
	neighs, err := netlink.NeighList(linkIndex, family)
	if err != nil {
		return nil, false, fmt.Errorf("netfabric: neighbor list on link %d: %v", linkIndex, err)
	}
	target := ip.Unmap()
	for _, n := range neighs {
		na, ok := netip.AddrFromSlice(n.IP)
		if !ok || na.Unmap() != target {
			continue
		}
		if len(n.HardwareAddr) != 6 {
			continue
		}
		if n.State&neighborResolvedStates == 0 {
			continue
		}
		return n.HardwareAddr, true, nil
	}
	return nil, false, nil
}

// probeNeighbor sends a single best-effort UDP datagram toward ip so the kernel
// resolves the on-link next hop, then waits briefly for the reply to land in the
// neighbor table. The probe socket is bound to the bridge with SO_BINDTODEVICE so
// the datagram egresses that interface regardless of the main route table: two
// overlays may carry the same guest subnet on different bridges, and a route-based
// send would otherwise provoke ARP on the wrong bridge and resolve nothing. Every
// error is ignored: the probe only nudges resolution, and the caller re-reads the
// table afterwards regardless.
func probeNeighbor(ip netip.Addr, bridge string) {
	d := net.Dialer{
		Timeout: neighborProbeTimeout,
		Control: BindToDeviceControl(bridge),
	}
	conn, err := d.Dial("udp", net.JoinHostPort(ip.String(), neighborProbePort))
	if err != nil {
		return
	}
	_ = conn.SetWriteDeadline(time.Now().Add(neighborProbeTimeout))
	_, _ = conn.Write([]byte{0})
	_ = conn.Close()
	time.Sleep(neighborProbeWait)
}
