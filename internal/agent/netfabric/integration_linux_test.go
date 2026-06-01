// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux && integration
// +build linux,integration

package netfabric

import (
	"bytes"
	"net/netip"
	"runtime"
	"testing"

	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// withNetNS runs fn inside a fresh, throwaway network namespace so the
// test never touches the host's real interfaces. It locks the goroutine
// to its OS thread for the duration, as required by the netns API. The
// test is skipped when the namespace cannot be created (typically a lack
// of CAP_NET_ADMIN); on Lima with root it runs for real.
func withNetNS(t *testing.T, fn func()) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	orig, err := netns.Get()
	if err != nil {
		t.Fatalf("netns.Get() = %v", err)
	}
	defer func() {
		if err := netns.Set(orig); err != nil {
			t.Errorf("restore netns: %v", err)
		}
		if err := orig.Close(); err != nil {
			t.Errorf("close orig netns: %v", err)
		}
	}()

	ns, err := netns.New()
	if err != nil {
		t.Skipf("create netns (needs CAP_NET_ADMIN): %v", err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			t.Errorf("close test netns: %v", err)
		}
	}()

	fn()
}

func TestLinuxFabricBridgeLifecycle(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		const name = "ot-br-test0"

		exists, err := f.BridgeExists(name)
		if err != nil {
			t.Fatalf("BridgeExists(%q) = %v", name, err)
		}
		if exists {
			t.Fatalf("BridgeExists(%q) = true before create, want false", name)
		}

		if err := f.EnsureBridge(name, 1450); err != nil {
			t.Fatalf("EnsureBridge(%q, 1450) = %v", name, err)
		}

		// Idempotent: a second EnsureBridge must succeed too.
		if err := f.EnsureBridge(name, 1450); err != nil {
			t.Fatalf("EnsureBridge(%q) second call = %v", name, err)
		}

		exists, err = f.BridgeExists(name)
		if err != nil {
			t.Fatalf("BridgeExists(%q) = %v", name, err)
		}
		if !exists {
			t.Fatalf("BridgeExists(%q) = false after create, want true", name)
		}

		link, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatalf("LinkByName(%q) = %v", name, err)
		}
		if mtu := link.Attrs().MTU; mtu != 1450 {
			t.Errorf("bridge MTU = %d, want 1450", mtu)
		}
		if link.Attrs().Flags&1 == 0 { // net.FlagUp == 1
			t.Errorf("bridge not up after EnsureBridge")
		}

		if err := f.RemoveBridge(name); err != nil {
			t.Fatalf("RemoveBridge(%q) = %v", name, err)
		}

		// Idempotent: removing an absent bridge is a no-op.
		if err := f.RemoveBridge(name); err != nil {
			t.Fatalf("RemoveBridge(%q) on absent = %v", name, err)
		}

		exists, err = f.BridgeExists(name)
		if err != nil {
			t.Fatalf("BridgeExists(%q) = %v", name, err)
		}
		if exists {
			t.Fatalf("BridgeExists(%q) = true after remove, want false", name)
		}
	})
}

func TestLinuxFabricTapLifecycle(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		const (
			bridge = "ot-br-tap0"
			tap    = "ottap0"
		)

		if err := f.EnsureBridge(bridge, 1500); err != nil {
			t.Fatalf("EnsureBridge(%q, 1500) = %v", bridge, err)
		}

		if err := f.CreateTap(tap, 1500); err != nil {
			t.Fatalf("CreateTap(%q, 1500) = %v", tap, err)
		}

		link, err := netlink.LinkByName(tap)
		if err != nil {
			t.Fatalf("LinkByName(%q) = %v", tap, err)
		}
		if _, ok := link.(*netlink.Tuntap); !ok {
			t.Errorf("tap %q is %T, want *netlink.Tuntap", tap, link)
		}
		if mtu := link.Attrs().MTU; mtu != 1500 {
			t.Errorf("tap MTU = %d, want 1500", mtu)
		}
		if link.Attrs().Flags&1 == 0 { // net.FlagUp == 1
			t.Errorf("tap not up after CreateTap")
		}

		if err := f.AttachTap(tap, bridge); err != nil {
			t.Fatalf("AttachTap(%q, %q) = %v", tap, bridge, err)
		}

		brLink, err := netlink.LinkByName(bridge)
		if err != nil {
			t.Fatalf("LinkByName(%q) = %v", bridge, err)
		}
		link, err = netlink.LinkByName(tap)
		if err != nil {
			t.Fatalf("LinkByName(%q) = %v", tap, err)
		}
		if got, want := link.Attrs().MasterIndex, brLink.Attrs().Index; got != want {
			t.Errorf("tap MasterIndex = %d, want %d (bridge index)", got, want)
		}

		// Idempotent: a second CreateTap must succeed too.
		if err := f.CreateTap(tap, 1500); err != nil {
			t.Fatalf("CreateTap(%q) second call = %v", tap, err)
		}

		if err := f.DeleteTap(tap); err != nil {
			t.Fatalf("DeleteTap(%q) = %v", tap, err)
		}

		// Idempotent: deleting an absent tap is a no-op.
		if err := f.DeleteTap(tap); err != nil {
			t.Fatalf("DeleteTap(%q) on absent = %v", tap, err)
		}

		if _, err := netlink.LinkByName(tap); err == nil {
			t.Fatalf("LinkByName(%q) succeeded after delete, want not-found", tap)
		}
	})
}

// countMasqRules returns how many rules in the otherix-nat postrouting
// chain carry the marker for subnet.
func countMasqRules(t *testing.T, subnet netip.Prefix) int {
	t.Helper()
	c := &nftables.Conn{}
	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: natTableName}
	chain := &nftables.Chain{Name: natChainName, Table: table}
	rules, err := c.GetRules(table, chain)
	if err != nil {
		t.Fatalf("GetRules(%s/%s) = %v", natTableName, natChainName, err)
	}
	marker := masqUserData(subnet)
	n := 0
	for _, r := range rules {
		if bytes.Equal(r.UserData, marker) {
			n++
		}
	}
	return n
}

func TestLinuxFabricNAT(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		const bridge = "ot-br-nat0"
		gw := netip.MustParsePrefix("10.99.0.1/24")
		subnet := netip.MustParsePrefix("10.99.0.0/24")

		if err := f.EnsureBridge(bridge, 1500); err != nil {
			t.Fatalf("EnsureBridge(%q, 1500) = %v", bridge, err)
		}

		if err := f.EnsureGatewayAddr(bridge, gw); err != nil {
			t.Fatalf("EnsureGatewayAddr(%q, %s) = %v", bridge, gw, err)
		}

		link, err := netlink.LinkByName(bridge)
		if err != nil {
			t.Fatalf("LinkByName(%q) = %v", bridge, err)
		}
		addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			t.Fatalf("AddrList(%q) = %v", bridge, err)
		}
		found := false
		for _, a := range addrs {
			if a.IPNet != nil && a.IPNet.String() == gw.String() {
				found = true
			}
		}
		if !found {
			t.Fatalf("gateway addr %s not present on %q after EnsureGatewayAddr", gw, bridge)
		}

		// Probe nftables support before asserting on masquerade. A fresh
		// netns without nft support (older kernels, missing modules) makes
		// the first masquerade op fail; skip rather than fail there.
		if err := f.EnsureMasquerade(subnet, "lo"); err != nil {
			t.Skipf("EnsureMasquerade(%s, lo) = %v (nftables unavailable in netns?)", subnet, err)
		}

		if n := countMasqRules(t, subnet); n != 1 {
			t.Fatalf("masquerade rule count = %d after first ensure, want 1", n)
		}

		// Idempotent: a second ensure must not add a duplicate rule.
		if err := f.EnsureMasquerade(subnet, "lo"); err != nil {
			t.Fatalf("EnsureMasquerade(%s, lo) second call = %v", subnet, err)
		}
		if n := countMasqRules(t, subnet); n != 1 {
			t.Fatalf("masquerade rule count = %d after second ensure, want 1", n)
		}

		// Only Otherix's own table must exist; the operator ruleset is
		// never touched.
		c := &nftables.Conn{}
		tables, err := c.ListTables()
		if err != nil {
			t.Fatalf("ListTables() = %v", err)
		}
		for _, tbl := range tables {
			if tbl.Name != natTableName {
				t.Errorf("unexpected nftables table %q (family %d), want only %q", tbl.Name, tbl.Family, natTableName)
			}
		}

		if err := f.RemoveMasquerade(subnet); err != nil {
			t.Fatalf("RemoveMasquerade(%s) = %v", subnet, err)
		}
		if n := countMasqRules(t, subnet); n != 0 {
			t.Fatalf("masquerade rule count = %d after remove, want 0", n)
		}

		// Idempotent: removing an absent rule is a no-op.
		if err := f.RemoveMasquerade(subnet); err != nil {
			t.Fatalf("RemoveMasquerade(%s) on absent = %v", subnet, err)
		}

		if err := f.RemoveGatewayAddr(bridge, gw); err != nil {
			t.Fatalf("RemoveGatewayAddr(%q, %s) = %v", bridge, gw, err)
		}
		addrs, err = netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			t.Fatalf("AddrList(%q) = %v", bridge, err)
		}
		for _, a := range addrs {
			if a.IPNet != nil && a.IPNet.String() == gw.String() {
				t.Fatalf("gateway addr %s still present after RemoveGatewayAddr", gw)
			}
		}

		// Idempotent: removing an absent addr is a no-op.
		if err := f.RemoveGatewayAddr(bridge, gw); err != nil {
			t.Fatalf("RemoveGatewayAddr(%q, %s) on absent = %v", bridge, gw, err)
		}
	})
}

// TestLinuxFabricRemoveMasqueradeOnFreshNetns asserts that RemoveMasquerade
// is idempotent on a fresh host: with no prior EnsureMasquerade the
// otherix-nat table has never been created, and removal must return nil
// without materialising the table.
func TestLinuxFabricRemoveMasqueradeOnFreshNetns(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		subnet := netip.MustParsePrefix("10.99.0.0/24")

		// Probe nftables support: ListTables fails on a netns without nft
		// support, so skip there rather than fail, consistent with the NAT
		// test above.
		c := &nftables.Conn{}
		if _, err := c.ListTables(); err != nil {
			t.Skipf("ListTables() = %v (nftables unavailable in netns?)", err)
		}

		if err := f.RemoveMasquerade(subnet); err != nil {
			t.Fatalf("RemoveMasquerade(%s) on fresh netns = %v, want nil", subnet, err)
		}

		// Removal must not have created the otherix-nat table.
		tables, err := c.ListTables()
		if err != nil {
			t.Fatalf("ListTables() = %v", err)
		}
		for _, tbl := range tables {
			if tbl.Name == natTableName {
				t.Fatalf("RemoveMasquerade created table %q, want absent", natTableName)
			}
		}
	})
}
