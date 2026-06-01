// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package netfabric

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// natTableName is the single nftables table Otherix owns. The fabric
// never reads, writes, or flushes any other table or chain, so the
// operator's own firewall ruleset is left untouched.
const natTableName = "otherix-nat"

// natChainName is the postrouting NAT chain inside natTableName where
// every masquerade rule lives.
const natChainName = "postrouting"

// ifnameSize is the fixed width of an nftables OIFNAME comparand,
// matching the kernel IFNAMSIZ. Interface names are NUL-padded to this
// width before comparison.
const ifnameSize = 16

// EnsureGatewayAddr assigns addr to the named bridge link, idempotently.
// AddrReplace adds the address when absent and is a no-op when it is
// already present, so a second call against the same bridge succeeds.
func (f *linuxFabric) EnsureGatewayAddr(bridge string, addr netip.Prefix) error {
	link, err := netlink.LinkByName(bridge)
	if err != nil {
		return fmt.Errorf("netfabric: ensure gateway addr on %s: %v", bridge, err)
	}
	a, err := netlink.ParseAddr(addr.String())
	if err != nil {
		return fmt.Errorf("netfabric: ensure gateway addr on %s: parse %s: %v", bridge, addr, err)
	}
	if err := netlink.AddrReplace(link, a); err != nil {
		return fmt.Errorf("netfabric: ensure gateway addr on %s: %v", bridge, err)
	}
	return nil
}

// RemoveGatewayAddr removes addr from the named bridge link. It returns
// nil when the bridge link is absent and when the address is already
// gone, so it is safe to call during repeated teardown.
func (f *linuxFabric) RemoveGatewayAddr(bridge string, addr netip.Prefix) error {
	link, err := netlink.LinkByName(bridge)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("netfabric: remove gateway addr on %s: %v", bridge, err)
	}
	a, err := netlink.ParseAddr(addr.String())
	if err != nil {
		return fmt.Errorf("netfabric: remove gateway addr on %s: parse %s: %v", bridge, addr, err)
	}
	if err := netlink.AddrDel(link, a); err != nil {
		if errors.Is(err, unix.EADDRNOTAVAIL) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("netfabric: remove gateway addr on %s: %v", bridge, err)
	}
	return nil
}

// masqUserData returns the stable UserData marker tagged onto the
// masquerade rule for subnet. Rules are matched on this marker for
// idempotent ensure and targeted removal, so it must not depend on the
// resolved egress interface.
func masqUserData(subnet netip.Prefix) []byte {
	return []byte("otherix:" + subnet.String())
}

// EnsureMasquerade installs a MASQUERADE rule for source traffic from
// subnet leaving via egressIface, idempotently. When egressIface is
// empty the host's default-route interface is used. The rule is tagged
// with a per-subnet UserData marker; a second call that finds a rule
// carrying that marker does nothing.
func (f *linuxFabric) EnsureMasquerade(subnet netip.Prefix, egressIface string) error {
	if !subnet.Addr().Is4() {
		return fmt.Errorf("netfabric: ensure masquerade for %s: only IPv4 subnets are supported", subnet)
	}
	if egressIface == "" {
		iface, err := defaultEgressIface()
		if err != nil {
			return fmt.Errorf("netfabric: ensure masquerade for %s: %v", subnet, err)
		}
		egressIface = iface
	}

	c := &nftables.Conn{}
	table, chain := f.natTableChain(c)
	// Materialise the table and chain in the kernel before the GetRules
	// precheck. On a fresh host the table has never existed, and GetRules
	// issues an immediate kernel dump that would return ENOENT; flushing
	// the create-or-noop first guarantees the dump finds the table.
	if err := c.Flush(); err != nil {
		return fmt.Errorf("netfabric: ensure masquerade for %s: %v", subnet, err)
	}

	marker := masqUserData(subnet)
	rules, err := c.GetRules(table, chain)
	if err != nil {
		return fmt.Errorf("netfabric: ensure masquerade for %s: list rules: %v", subnet, err)
	}
	for _, r := range rules {
		if bytes.Equal(r.UserData, marker) {
			return nil
		}
	}

	c.AddRule(&nftables.Rule{
		Table:    table,
		Chain:    chain,
		UserData: marker,
		Exprs:    masqExprs(subnet, egressIface),
	})
	if err := c.Flush(); err != nil {
		return fmt.Errorf("netfabric: ensure masquerade for %s: %v", subnet, err)
	}
	return nil
}

// RemoveMasquerade removes every masquerade rule tagged for subnet from
// Otherix's own NAT chain. It returns nil when no matching rule exists,
// including when the NAT table has never been created; removal never
// materialises state.
func (f *linuxFabric) RemoveMasquerade(subnet netip.Prefix) error {
	c := &nftables.Conn{}
	// Reference the table and chain without creating them. On a fresh host
	// the GetRules dump returns ENOENT for the absent table; that is "no
	// rules", not an error, so the removal stays idempotent.
	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: natTableName}
	chain := &nftables.Chain{Name: natChainName, Table: table}

	marker := masqUserData(subnet)
	rules, err := c.GetRules(table, chain)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("netfabric: remove masquerade for %s: list rules: %v", subnet, err)
	}
	deleted := false
	for _, r := range rules {
		if !bytes.Equal(r.UserData, marker) {
			continue
		}
		r.Table = table
		r.Chain = chain
		if err := c.DelRule(r); err != nil {
			return fmt.Errorf("netfabric: remove masquerade for %s: %v", subnet, err)
		}
		deleted = true
	}
	if !deleted {
		return nil
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("netfabric: remove masquerade for %s: %v", subnet, err)
	}
	return nil
}

// natTableChain returns get-or-create handles for Otherix's own NAT
// table and postrouting chain. AddTable and AddChain are create-or-noop
// at the netlink layer, so calling them on every operation keeps the
// objects present without disturbing any other table.
func (f *linuxFabric) natTableChain(c *nftables.Conn) (*nftables.Table, *nftables.Chain) {
	table := c.AddTable(&nftables.Table{
		Family: nftables.TableFamilyIPv4,
		Name:   natTableName,
	})
	chain := c.AddChain(&nftables.Chain{
		Name:     natChainName,
		Table:    table,
		Type:     nftables.ChainTypeNAT,
		Hooknum:  nftables.ChainHookPostrouting,
		Priority: nftables.ChainPriorityNATSource,
	})
	return table, chain
}

// masqExprs builds the expression sequence for a masquerade rule that
// matches IPv4 source addresses inside subnet egressing via iface. It
// loads the IPv4 source address (network header offset 12, length 4),
// masks it to the prefix, compares it to the network address, compares
// the outbound interface name, then masquerades.
func masqExprs(subnet netip.Prefix, iface string) []expr.Any {
	network := subnet.Masked().Addr().As4()
	mask := prefixMask4(subnet.Bits())
	return []expr.Any{
		// payload load 4b @ network header + 12 => reg 1 (ip saddr)
		&expr.Payload{
			DestRegister: 1,
			Base:         expr.PayloadBaseNetworkHeader,
			Offset:       12,
			Len:          4,
		},
		// bitwise reg 1 = (reg 1 & mask) ^ 0
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           mask[:],
			Xor:            []byte{0, 0, 0, 0},
		},
		// cmp eq reg 1 network
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     network[:],
		},
		// meta load oifname => reg 1
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		// cmp eq reg 1 iface
		&expr.Cmp{
			Op:       expr.CmpOpEq,
			Register: 1,
			Data:     ifnameComparand(iface),
		},
		// masq
		&expr.Masq{},
	}
}

// prefixMask4 returns the 4-byte big-endian network mask for an IPv4
// prefix of the given length in bits.
func prefixMask4(bits int) [4]byte {
	var m uint32
	if bits > 0 {
		m = ^uint32(0) << (32 - bits)
	}
	return [4]byte{
		byte(m >> 24),
		byte(m >> 16),
		byte(m >> 8),
		byte(m),
	}
}

// ifnameComparand returns name NUL-padded to the kernel IFNAMSIZ width
// for comparison against an OIFNAME meta load.
func ifnameComparand(name string) []byte {
	b := make([]byte, ifnameSize)
	copy(b, name)
	return b
}

// defaultEgressIface returns the name of the interface carrying the
// host's IPv4 default route. It is used when EnsureMasquerade is called
// without an explicit egress interface.
func defaultEgressIface() (string, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return "", fmt.Errorf("list ipv4 routes: %v", err)
	}
	for _, r := range routes {
		if r.Dst != nil {
			continue
		}
		link, err := netlink.LinkByIndex(r.LinkIndex)
		if err != nil {
			return "", fmt.Errorf("resolve default-route link: %v", err)
		}
		return link.Attrs().Name, nil
	}
	return "", errors.New("no ipv4 default route found")
}
