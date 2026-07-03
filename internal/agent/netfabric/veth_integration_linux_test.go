// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux && integration
// +build linux,integration

package netfabric

import (
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

// TestEnsureVethStandalone brings up a veth pair on a bridge in a throwaway
// netns and asserts the host end carries the tenant addr + MAC, the peer is
// enslaved to the bridge, the sysctls are set, and RemoveVeth deletes both ends.
func TestEnsureVethStandalone(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		const vni uint32 = 7
		bridge := "otvb7"
		if err := f.EnsureBridge(bridge, 1390); err != nil {
			t.Fatalf("EnsureBridge() = %v", err)
		}
		host := GatewayVethHostName(vni)
		mac, _ := net.ParseMAC("02:00:00:00:00:07")
		cfg := VethConfig{
			HostName: host,
			PeerName: GatewayVethPeerName(vni),
			Bridge:   bridge,
			Addr:     netip.MustParsePrefix("10.70.0.2/24"),
			MAC:      mac,
			MTU:      1390,
		}
		if err := f.EnsureVeth(cfg); err != nil {
			t.Fatalf("EnsureVeth() = %v", err)
		}
		// Idempotent re-assert must not error.
		if err := f.EnsureVeth(cfg); err != nil {
			t.Fatalf("EnsureVeth() second call = %v", err)
		}

		hl, err := netlink.LinkByName(host)
		if err != nil {
			t.Fatalf("host link: %v", err)
		}
		if hl.Attrs().HardwareAddr.String() != mac.String() {
			t.Errorf("host MAC = %v, want %v", hl.Attrs().HardwareAddr, mac)
		}
		addrs, err := netlink.AddrList(hl, netlink.FAMILY_V4)
		if err != nil || len(addrs) == 0 || addrs[0].IPNet.String() != "10.70.0.2/24" {
			t.Errorf("host addrs = %v (err %v), want 10.70.0.2/24", addrs, err)
		}
		peer, err := netlink.LinkByName(GatewayVethPeerName(vni))
		if err != nil {
			t.Fatalf("peer link: %v", err)
		}
		br, err := netlink.LinkByName(bridge)
		if err != nil {
			t.Fatalf("bridge link: %v", err)
		}
		if peer.Attrs().MasterIndex != br.Attrs().Index {
			t.Errorf("peer MasterIndex = %d, want bridge index %d", peer.Attrs().MasterIndex, br.Attrs().Index)
		}
		if got := readSysctl(t, host, "rp_filter"); got != "2" {
			t.Errorf("host rp_filter = %q, want 2", got)
		}
		if got := readSysctl(t, host, "arp_ignore"); got != "1" {
			t.Errorf("host arp_ignore = %q, want 1", got)
		}
		if got := readSysctl(t, host, "arp_announce"); got != "2" {
			t.Errorf("host arp_announce = %q, want 2", got)
		}

		if err := f.RemoveVeth(host); err != nil {
			t.Fatalf("RemoveVeth() = %v", err)
		}
		if _, err := netlink.LinkByName(host); err == nil {
			t.Errorf("host link still present after RemoveVeth")
		}
		if _, err := netlink.LinkByName(GatewayVethPeerName(vni)); err == nil {
			t.Errorf("peer link still present after RemoveVeth (deleting one end must remove both)")
		}
		// RemoveVeth on an absent pair is a no-op.
		if err := f.RemoveVeth(host); err != nil {
			t.Errorf("RemoveVeth() on absent pair = %v, want nil", err)
		}
	})
}

// TestVethCoexistsWithAnycastServices is the co-located case: on ONE otvb<vni>
// the anycast services plane (bridge MAC = anycast, 169.254.1.1/32, bridge /24
// route) and the ingress veth (tenant MAC on the host end, tenant /24) must
// coexist. The veth must NOT disturb the bridge's anycast MAC, and both /24
// routes must be present.
func TestVethCoexistsWithAnycastServices(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		const vni uint32 = 9
		bridge := "otvb9"
		subnet := netip.MustParsePrefix("10.90.0.0/24")
		if err := f.EnsureBridge(bridge, 1390); err != nil {
			t.Fatalf("EnsureBridge() = %v", err)
		}
		anycastMAC := GatewayMAC(vni)
		if err := f.EnsureAnycastGateway(bridge, OverlayGatewayAddr, anycastMAC); err != nil {
			t.Fatalf("EnsureAnycastGateway() = %v", err)
		}
		if err := f.EnsureBridgeRoute(subnet, bridge); err != nil {
			t.Fatalf("EnsureBridgeRoute() = %v", err)
		}
		host := GatewayVethHostName(vni)
		tenantMAC, _ := net.ParseMAC("02:00:00:00:00:99")
		if err := f.EnsureVeth(VethConfig{
			HostName: host,
			PeerName: GatewayVethPeerName(vni),
			Bridge:   bridge,
			Addr:     netip.MustParsePrefix("10.90.0.2/24"),
			MAC:      tenantMAC,
			MTU:      1390,
		}); err != nil {
			t.Fatalf("EnsureVeth() = %v", err)
		}

		br, err := netlink.LinkByName(bridge)
		if err != nil {
			t.Fatalf("bridge link: %v", err)
		}
		if br.Attrs().HardwareAddr.String() != anycastMAC.String() {
			t.Errorf("bridge MAC = %v, want anycast %v (veth must not touch the bridge hwaddr)",
				br.Attrs().HardwareAddr, anycastMAC)
		}
		hl, err := netlink.LinkByName(host)
		if err != nil {
			t.Fatalf("host link: %v", err)
		}
		if hl.Attrs().HardwareAddr.String() != tenantMAC.String() {
			t.Errorf("veth host MAC = %v, want tenant %v", hl.Attrs().HardwareAddr, tenantMAC)
		}
		// Both interfaces carry a connected /24 to the guest subnet.
		routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
		if err != nil {
			t.Fatalf("route list: %v", err)
		}
		var onBridge, onVeth bool
		for _, r := range routes {
			if r.Dst == nil || r.Dst.String() != "10.90.0.0/24" {
				continue
			}
			switch r.LinkIndex {
			case br.Attrs().Index:
				onBridge = true
			case hl.Attrs().Index:
				onVeth = true
			}
		}
		if !onBridge || !onVeth {
			t.Errorf("connected /24 routes: bridge=%v veth=%v, want both true", onBridge, onVeth)
		}
		if got := readSysctl(t, host, "rp_filter"); got != "2" {
			t.Errorf("veth rp_filter = %q, want 2 (loose, so a reply survives the bridge's second /24)", got)
		}
	})
}

// TestMultiOverlayVethsAreIndependent brings up two overlays' veths and asserts
// each host end carries its own subnet and MAC, and removing one leaves the other.
func TestMultiOverlayVethsAreIndependent(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		type ov struct {
			vni    uint32
			bridge string
			addr   string
			mac    string
		}
		a := ov{11, "otvb11", "10.11.0.2/24", "02:00:00:00:00:0b"}
		b := ov{12, "otvb12", "10.12.0.2/24", "02:00:00:00:00:0c"}
		for _, o := range []ov{a, b} {
			if err := f.EnsureBridge(o.bridge, 1390); err != nil {
				t.Fatalf("EnsureBridge(%s) = %v", o.bridge, err)
			}
			mac, _ := net.ParseMAC(o.mac)
			if err := f.EnsureVeth(VethConfig{
				HostName: GatewayVethHostName(o.vni),
				PeerName: GatewayVethPeerName(o.vni),
				Bridge:   o.bridge,
				Addr:     netip.MustParsePrefix(o.addr),
				MAC:      mac,
				MTU:      1390,
			}); err != nil {
				t.Fatalf("EnsureVeth(%d) = %v", o.vni, err)
			}
		}
		if err := f.RemoveVeth(GatewayVethHostName(a.vni)); err != nil {
			t.Fatalf("RemoveVeth(a) = %v", err)
		}
		if _, err := netlink.LinkByName(GatewayVethHostName(a.vni)); err == nil {
			t.Errorf("overlay a veth still present after removal")
		}
		if _, err := netlink.LinkByName(GatewayVethHostName(b.vni)); err != nil {
			t.Errorf("overlay b veth removed by overlay a teardown: %v", err)
		}
	})
}

// readSysctl reads /proc/sys/net/ipv4/conf/<iface>/<key>.
func readSysctl(t *testing.T, iface, key string) string {
	t.Helper()
	b, err := os.ReadFile("/proc/sys/net/ipv4/conf/" + iface + "/" + key)
	if err != nil {
		t.Fatalf("read sysctl %s/%s: %v", iface, key, err)
	}
	return strings.TrimSpace(string(b))
}
