// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux
// +build linux

package netfabric

import (
	"bytes"
	"errors"
	"fmt"
	"net"
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

// EnableIPForwarding enables IPv4 forwarding in the current network
// namespace. net.ipv4.ip_forward is per-netns, so writing it from the agent
// process (which runs in the node's netns) affects only this node. Idempotent:
// writing "1" when already 1 is a no-op.
func (f *linuxFabric) EnableIPForwarding() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// 0o600: procfs ignores the mode (the file already exists), but gosec G306
	// wants <=0o600. Functionally irrelevant for /proc/sys.
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o600); err != nil {
		return fmt.Errorf("netfabric: enable ip_forward: %v", err)
	}
	return nil
}

// writeIfaceSysctl writes value to /proc/sys/net/ipv4/conf/<iface>/<key>,
// the per-interface IPv4 sysctl tree. Used to contain the anycast gateway's
// shared link-local address to its own bridge.
func writeIfaceSysctl(iface, key, value string) error {
	// iface is an Otherix-managed bridge name (otb<vni>), and key is a fixed
	// procfs leaf - not user input.
	path := "/proc/sys/net/ipv4/conf/" + iface + "/" + key //nolint:gosec // G304: fixed procfs path, Otherix-owned iface name
	// 0o600: procfs ignores the mode (the file pre-exists); gosec G306 wants <=0o600.
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %v", path, err)
	}
	return nil
}

// EnsureAnycastGateway pins mac on the bridge link and assigns addr/32 to it,
// idempotently. Setting the bridge hardware address explicitly stops the
// kernel auto-inheriting the lowest enslaved-port MAC, keeping the gateway MAC
// stable; a re-assert each reconcile pass is harmless.
func (f *linuxFabric) EnsureAnycastGateway(bridge string, addr netip.Addr, mac net.HardwareAddr) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	link, err := netlink.LinkByName(bridge)
	if err != nil {
		return fmt.Errorf("netfabric: ensure anycast gateway on %s: %v", bridge, err)
	}
	if err := netlink.LinkSetHardwareAddr(link, mac); err != nil {
		return fmt.Errorf("netfabric: ensure anycast gateway on %s: set mac: %v", bridge, err)
	}
	p := netip.PrefixFrom(addr, addr.BitLen()) // /32 for IPv4
	a, err := netlink.ParseAddr(p.String())
	if err != nil {
		return fmt.Errorf("netfabric: ensure anycast gateway on %s: parse %s: %v", bridge, p, err)
	}
	if err := netlink.AddrReplace(link, a); err != nil {
		return fmt.Errorf("netfabric: ensure anycast gateway on %s: %v", bridge, err)
	}
	// Anycast containment: the same link-local /32 lives on every overlay
	// bridge, so restrict ARP replies to the addressed interface (arp_ignore=1)
	// and force best-local-source announcements (arp_announce=2). This stops a
	// VM on one overlay from learning another bridge's gateway MAC.
	if err := writeIfaceSysctl(bridge, "arp_ignore", "1"); err != nil {
		return fmt.Errorf("netfabric: ensure anycast gateway on %s: %v", bridge, err)
	}
	if err := writeIfaceSysctl(bridge, "arp_announce", "2"); err != nil {
		return fmt.Errorf("netfabric: ensure anycast gateway on %s: %v", bridge, err)
	}
	return nil
}

// RemoveAnycastGateway removes addr/32 from the bridge. It returns nil when the
// bridge link is absent and when the address is already gone, mirroring
// RemoveGatewayAddr, so repeated teardown is safe.
func (f *linuxFabric) RemoveAnycastGateway(bridge string, addr netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	link, err := netlink.LinkByName(bridge)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("netfabric: remove anycast gateway on %s: %v", bridge, err)
	}
	p := netip.PrefixFrom(addr, addr.BitLen())
	a, err := netlink.ParseAddr(p.String())
	if err != nil {
		return fmt.Errorf("netfabric: remove anycast gateway on %s: parse %s: %v", bridge, p, err)
	}
	if err := netlink.AddrDel(link, a); err != nil {
		if errors.Is(err, unix.EADDRNOTAVAIL) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("netfabric: remove anycast gateway on %s: %v", bridge, err)
	}
	return nil
}

// EnsureGatewayAddr assigns addr to the named bridge link, idempotently.
// AddrReplace adds the address when absent and is a no-op when it is
// already present, so a second call against the same bridge succeeds.
func (f *linuxFabric) EnsureGatewayAddr(bridge string, addr netip.Prefix) error {
	f.mu.Lock()
	defer f.mu.Unlock()

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
	f.mu.Lock()
	defer f.mu.Unlock()

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
// masquerade rule for subnet egressing via iface. The marker is keyed on
// both subnet and the resolved egress interface so that changing the
// egress interface installs a distinct rule rather than silently matching
// the stale one. masqSubnetPrefix returns the iface-independent prefix of
// this marker, used by RemoveMasquerade to remove every rule for a subnet
// regardless of which iface it was installed for.
func masqUserData(subnet netip.Prefix, iface string) []byte {
	return append(masqSubnetPrefix(subnet), iface...)
}

// masqSubnetPrefix returns the leading "otherix:<subnet>:" segment shared
// by every masquerade marker for subnet, independent of the egress iface.
func masqSubnetPrefix(subnet netip.Prefix) []byte {
	return []byte("otherix:" + subnet.String() + ":")
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
	// Guard the resolved iface against the kernel IFNAMSIZ ceiling. An
	// over-long name would otherwise be silently truncated into the
	// OIFNAME comparand, producing a rule that matches the wrong (or no)
	// interface. The terminating NUL leaves ifnameSize-1 usable octets.
	if len(egressIface) >= ifnameSize {
		return fmt.Errorf("netfabric: ensure masquerade for %s: egress iface %q exceeds %d-byte limit", subnet, egressIface, ifnameSize-1)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	c := &nftables.Conn{}
	table, chain := f.natTableChain(c)
	// Materialise the table and chain in the kernel before the GetRules
	// precheck. On a fresh host the table has never existed, and GetRules
	// issues an immediate kernel dump that would return ENOENT; flushing
	// the create-or-noop first guarantees the dump finds the table. This
	// Flush is load-bearing - see TestLinuxFabricNAT on a fresh netns.
	if err := c.Flush(); err != nil {
		return fmt.Errorf("netfabric: ensure masquerade for %s: %v", subnet, err)
	}

	marker := masqUserData(subnet, egressIface)
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
	f.mu.Lock()
	defer f.mu.Unlock()

	c := &nftables.Conn{}
	// Reference the table and chain without creating them. On a fresh host
	// the GetRules dump returns ENOENT for the absent table; that is "no
	// rules", not an error, so the removal stays idempotent.
	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: natTableName}
	chain := &nftables.Chain{Name: natChainName, Table: table}

	// Match on the iface-independent subnet prefix so every rule for the
	// subnet is removed regardless of which egress iface it was installed
	// for. The marker is "otherix:<subnet>:<iface>"; the prefix is
	// "otherix:<subnet>:".
	prefix := masqSubnetPrefix(subnet)
	rules, err := c.GetRules(table, chain)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("netfabric: remove masquerade for %s: list rules: %v", subnet, err)
	}
	deleted := false
	for _, r := range rules {
		if !bytes.HasPrefix(r.UserData, prefix) {
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

// masqIfaceUserData returns the stable UserData marker for an interface-keyed
// masquerade rule (inIface egressing via egressIface). masqIfacePrefix returns
// the egress-iface-independent prefix so RemoveMasqueradeIface can drop every
// rule for an inIface regardless of which egress iface it resolved to.
func masqIfaceUserData(inIface, egressIface string) []byte {
	return append(masqIfacePrefix(inIface), egressIface...)
}

// masqIfacePrefix returns the leading "otherix:iif:<inIface>:" marker segment.
func masqIfacePrefix(inIface string) []byte {
	return []byte("otherix:iif:" + inIface + ":")
}

// masqIfaceExprs builds the expression sequence for a masquerade rule matching
// traffic that ENTERED via inIface and LEAVES via egressIface, then
// masquerades. Unlike masqExprs it is CIDR-independent: it identifies overlay
// VM traffic by the ingress bridge, not by source subnet. Inter-overlay
// traffic leaves via otwg0/otvx*, not the uplink, so it never matches.
func masqIfaceExprs(inIface, egressIface string) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifnameComparand(inIface)},
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: ifnameComparand(egressIface)},
		&expr.Masq{},
	}
}

// EnsureMasqueradeIface installs a MASQUERADE rule for traffic entering via
// inIface and leaving via egressIface, idempotently. When egressIface is empty
// the host's default-route interface is used. The rule carries a per-inIface
// UserData marker; a second call that finds it does nothing.
func (f *linuxFabric) EnsureMasqueradeIface(inIface, egressIface string) error {
	if egressIface == "" {
		iface, err := defaultEgressIface()
		if err != nil {
			return fmt.Errorf("netfabric: ensure masquerade for iif %s: %v", inIface, err)
		}
		egressIface = iface
	}
	if len(inIface) >= ifnameSize || len(egressIface) >= ifnameSize {
		return fmt.Errorf("netfabric: ensure masquerade for iif %s: iface name exceeds %d-byte limit", inIface, ifnameSize-1)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	c := &nftables.Conn{}
	table, chain := f.natTableChain(c)
	// Flush is load-bearing on a fresh host: it materialises the table before
	// the GetRules dump, which would otherwise ENOENT. See masqExprs sibling.
	if err := c.Flush(); err != nil {
		return fmt.Errorf("netfabric: ensure masquerade for iif %s: %v", inIface, err)
	}

	marker := masqIfaceUserData(inIface, egressIface)
	prefix := masqIfacePrefix(inIface)
	rules, err := c.GetRules(table, chain)
	if err != nil {
		return fmt.Errorf("netfabric: ensure masquerade for iif %s: list rules: %v", inIface, err)
	}
	found := false
	changed := false
	for _, r := range rules {
		if !bytes.HasPrefix(r.UserData, prefix) {
			continue
		}
		if bytes.Equal(r.UserData, marker) {
			found = true
			continue
		}
		// Stale rule for this ingress bridge with a different egress iface
		// (the node's default route changed). Prune it so iface-keyed rules do
		// not accumulate across uplink changes.
		r.Table = table
		r.Chain = chain
		if err := c.DelRule(r); err != nil {
			return fmt.Errorf("netfabric: ensure masquerade for iif %s: prune stale: %v", inIface, err)
		}
		changed = true
	}
	if !found {
		c.AddRule(&nftables.Rule{
			Table:    table,
			Chain:    chain,
			UserData: marker,
			Exprs:    masqIfaceExprs(inIface, egressIface),
		})
		changed = true
	}
	if changed {
		if err := c.Flush(); err != nil {
			return fmt.Errorf("netfabric: ensure masquerade for iif %s: %v", inIface, err)
		}
	}
	return nil
}

// RemoveMasqueradeIface removes every interface-keyed masquerade rule tagged
// for inIface, regardless of the egress iface it resolved to. nil when none
// exists, including when the NAT table was never created.
func (f *linuxFabric) RemoveMasqueradeIface(inIface string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	c := &nftables.Conn{}
	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: natTableName}
	chain := &nftables.Chain{Name: natChainName, Table: table}

	prefix := masqIfacePrefix(inIface)
	rules, err := c.GetRules(table, chain)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("netfabric: remove masquerade for iif %s: list rules: %v", inIface, err)
	}
	deleted := false
	for _, r := range rules {
		if !bytes.HasPrefix(r.UserData, prefix) {
			continue
		}
		r.Table = table
		r.Chain = chain
		if err := c.DelRule(r); err != nil {
			return fmt.Errorf("netfabric: remove masquerade for iif %s: %v", inIface, err)
		}
		deleted = true
	}
	if !deleted {
		return nil
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("netfabric: remove masquerade for iif %s: %v", inIface, err)
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
		// A default route has no RTA_DST: depending on the kernel and the
		// netlink version that surfaces either as a nil Dst or as an
		// explicit 0.0.0.0/0 (zero-length mask, unspecified address).
		// Treat both as the default.
		if r.Dst != nil {
			ones, _ := r.Dst.Mask.Size()
			if ones != 0 || !r.Dst.IP.IsUnspecified() {
				continue
			}
		}
		link, err := netlink.LinkByIndex(r.LinkIndex)
		if err != nil {
			return "", fmt.Errorf("resolve default-route link: %v", err)
		}
		return link.Attrs().Name, nil
	}
	return "", errors.New("no ipv4 default route found")
}
