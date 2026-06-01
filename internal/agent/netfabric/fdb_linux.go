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
	"os"
	"sort"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// vxlanIndex resolves the otvx<vni> VTEP's link index, returning a clear
// error when the device is absent or is not a VXLAN (FDB programming needs
// the VTEP to exist first).
func (f *linuxFabric) vxlanIndex(vni uint32) (int, error) {
	name := vxlanName(vni)
	link, err := netlink.LinkByName(name)
	if err != nil {
		return 0, fmt.Errorf("netfabric: fdb on %s: %v", name, err)
	}
	if _, ok := link.(*netlink.Vxlan); !ok {
		return 0, fmt.Errorf("netfabric: fdb on %s: link is type %T, not a vxlan", name, link)
	}
	return link.Attrs().Index, nil
}

// fdbNeigh builds the kernel FDB neighbour for a MAC -> dst VTEP mapping on
// the VTEP at linkIndex: an AF_BRIDGE, NUD_PERMANENT, NTF_SELF entry whose
// IP carries the remote VTEP address.
func fdbNeigh(linkIndex int, e FDBEntry) *netlink.Neigh {
	return &netlink.Neigh{
		LinkIndex:    linkIndex,
		Family:       unix.AF_BRIDGE,
		State:        netlink.NUD_PERMANENT,
		Flags:        netlink.NTF_SELF,
		IP:           net.IP(e.Dst.Unmap().AsSlice()),
		HardwareAddr: e.MAC,
	}
}

// isZeroMAC reports whether mac is all zeros - the kernel's default/BUM FDB
// placeholder, which FDBList omits.
func isZeroMAC(mac net.HardwareAddr) bool {
	for _, b := range mac {
		if b != 0 {
			return false
		}
	}
	return true
}

// FDBAppend installs a MAC -> dst VTEP entry in the otvx<vni> VTEP's kernel
// FDB. It is idempotent: an identical entry already present is a no-op,
// mirroring EnsureMasquerade's list-then-add discipline.
func (f *linuxFabric) FDBAppend(vni uint32, e FDBEntry) error {
	if len(e.MAC) != 6 {
		return fmt.Errorf("netfabric: fdb append on vxlan %d: mac %q is not a 6-octet address", vni, e.MAC)
	}
	if !e.Dst.IsValid() {
		return fmt.Errorf("netfabric: fdb append on vxlan %d: invalid dst addr", vni)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	idx, err := f.vxlanIndex(vni)
	if err != nil {
		return err
	}
	existing, err := netlink.NeighList(idx, unix.AF_BRIDGE)
	if err != nil {
		return fmt.Errorf("netfabric: fdb append on vxlan %d: list: %v", vni, err)
	}
	dst := net.IP(e.Dst.Unmap().AsSlice())
	for _, n := range existing {
		if n.HardwareAddr.String() == e.MAC.String() && n.IP.Equal(dst) {
			return nil
		}
	}
	if err := netlink.NeighAppend(fdbNeigh(idx, e)); err != nil {
		return fmt.Errorf("netfabric: fdb append on vxlan %d: %v", vni, err)
	}
	return nil
}

// FDBDelete removes a MAC -> dst VTEP entry from the otvx<vni> VTEP's FDB.
// It returns nil when the entry is already absent.
func (f *linuxFabric) FDBDelete(vni uint32, e FDBEntry) error {
	if len(e.MAC) != 6 {
		return fmt.Errorf("netfabric: fdb delete on vxlan %d: mac %q is not a 6-octet address", vni, e.MAC)
	}
	if !e.Dst.IsValid() {
		return fmt.Errorf("netfabric: fdb delete on vxlan %d: invalid dst addr", vni)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	idx, err := f.vxlanIndex(vni)
	if err != nil {
		return err
	}
	if err := netlink.NeighDel(fdbNeigh(idx, e)); err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("netfabric: fdb delete on vxlan %d: %v", vni, err)
	}
	return nil
}

// FDBList returns the MAC -> dst VTEP entries in the otvx<vni> VTEP's FDB,
// sorted by MAC. The kernel's all-zeros default entry (the BUM / head-end
// placeholder) is skipped, so only real MAC -> dst mappings are returned.
// Learning is off on every VTEP we create, so every remaining entry is one we
// programmed; if N2/N3 ever enables learning, this must additionally filter on
// State&NUD_PERMANENT before the result is treated as the authoritative set.
func (f *linuxFabric) FDBList(vni uint32) ([]FDBEntry, error) {
	idx, err := f.vxlanIndex(vni)
	if err != nil {
		return nil, err
	}
	neighs, err := netlink.NeighList(idx, unix.AF_BRIDGE)
	if err != nil {
		return nil, fmt.Errorf("netfabric: fdb list on vxlan %d: %v", vni, err)
	}
	var out []FDBEntry
	for _, n := range neighs {
		if len(n.HardwareAddr) != 6 || isZeroMAC(n.HardwareAddr) {
			continue
		}
		dst, ok := netip.AddrFromSlice(n.IP)
		if !ok {
			continue
		}
		out = append(out, FDBEntry{MAC: n.HardwareAddr, Dst: dst.Unmap()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MAC.String() < out[j].MAC.String() })
	return out, nil
}
