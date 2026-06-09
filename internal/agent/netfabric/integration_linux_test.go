// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux && integration
// +build linux,integration

package netfabric

import (
	"bytes"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/nftables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// requireNetfabric handles an unavailable data-plane capability (no
// CAP_NET_ADMIN, no nftables, no wireguard module). By default - on
// developer laptops and other unprivileged environments - it SKIPS the
// test, matching the historical t.Skipf behaviour. When
// OTHERIX_NETFABRIC_REQUIRE is set, as the privileged CI netns job does,
// it FAILS instead: a regression that silently strips the host's
// data-plane capabilities must not pass green by skipping the whole
// suite (the gap this CI job was added to close). Like t.Skipf / t.Fatalf
// it never returns - both halt the calling test via runtime.Goexit.
func requireNetfabric(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("OTHERIX_NETFABRIC_REQUIRE") != "" {
		t.Fatalf("OTHERIX_NETFABRIC_REQUIRE set but a data-plane capability is unavailable: "+format, args...)
	}
	t.Skipf(format, args...)
}

// withNetNS runs fn inside a fresh, throwaway network namespace so the
// test never touches the host's real interfaces. It locks the goroutine
// to its OS thread for the duration, as required by the netns API. The
// test is skipped when the namespace cannot be created (typically a lack
// of CAP_NET_ADMIN); on Lima with root it runs for real. Under
// OTHERIX_NETFABRIC_REQUIRE that skip becomes a hard failure.
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
		requireNetfabric(t, "create netns (needs CAP_NET_ADMIN): %v", err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			t.Errorf("close test netns: %v", err)
		}
	}()

	fn()
}

// bringLoopbackUp configures 127.0.0.1/8 on lo and brings it up. A fresh
// netns has lo DOWN with no address, and a loopback-bound VTEP needs its
// source address to exist on an up interface.
func bringLoopbackUp(t *testing.T) {
	t.Helper()
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatalf("LinkByName(lo) = %v", err)
	}
	addr, err := netlink.ParseAddr("127.0.0.1/8")
	if err != nil {
		t.Fatalf("ParseAddr(127.0.0.1/8) = %v", err)
	}
	if err := netlink.AddrReplace(lo, addr); err != nil {
		t.Fatalf("AddrReplace(lo, 127.0.0.1/8) = %v", err)
	}
	if err := netlink.LinkSetUp(lo); err != nil {
		t.Fatalf("LinkSetUp(lo) = %v", err)
	}
}

// vxlanLoopbackConfig is the shared N1b scaffold VTEP config: VNI 1000 bound
// to loopback on the IANA VXLAN port with the overlay inner MTU.
func vxlanLoopbackConfig() VXLANConfig {
	return VXLANConfig{VNI: 1000, Local: netip.MustParseAddr("127.0.0.1"), Port: 4789, MTU: 1390}
}

func TestLinuxFabricVXLANLifecycle(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		const vni = 1000
		bringLoopbackUp(t)

		exists, err := f.VXLANExists(vni)
		if err != nil {
			t.Fatalf("VXLANExists(%d) = %v", vni, err)
		}
		if exists {
			t.Fatalf("VXLANExists(%d) = true before create, want false", vni)
		}

		cfg := vxlanLoopbackConfig()
		if err := f.EnsureVXLAN(cfg); err != nil {
			t.Fatalf("EnsureVXLAN(%+v) = %v", cfg, err)
		}
		// Idempotent: a second EnsureVXLAN must succeed.
		if err := f.EnsureVXLAN(cfg); err != nil {
			t.Fatalf("EnsureVXLAN second call = %v", err)
		}

		exists, err = f.VXLANExists(vni)
		if err != nil || !exists {
			t.Fatalf("VXLANExists(%d) after create = (%v, %v), want (true, nil)", vni, exists, err)
		}

		link, err := netlink.LinkByName("otvx1000")
		if err != nil {
			t.Fatalf("LinkByName(otvx1000) = %v", err)
		}
		vx, ok := link.(*netlink.Vxlan)
		if !ok {
			t.Fatalf("otvx1000 is %T, want *netlink.Vxlan", link)
		}
		if vx.VxlanId != 1000 {
			t.Errorf("VxlanId = %d, want 1000", vx.VxlanId)
		}
		if vx.Learning {
			t.Errorf("Learning = true, want false (controller-authoritative FDB)")
		}
		if vx.Port != 4789 {
			t.Errorf("Port = %d, want 4789", vx.Port)
		}
		if !vx.SrcAddr.Equal(net.ParseIP("127.0.0.1")) {
			t.Errorf("SrcAddr = %v, want 127.0.0.1", vx.SrcAddr)
		}
		if mtu := vx.Attrs().MTU; mtu != 1390 {
			t.Errorf("MTU = %d, want 1390", mtu)
		}

		if err := f.RemoveVXLAN(vni); err != nil {
			t.Fatalf("RemoveVXLAN(%d) = %v", vni, err)
		}
		// Idempotent: removing an absent VTEP is a no-op.
		if err := f.RemoveVXLAN(vni); err != nil {
			t.Fatalf("RemoveVXLAN(%d) on absent = %v", vni, err)
		}
		exists, err = f.VXLANExists(vni)
		if err != nil || exists {
			t.Fatalf("VXLANExists(%d) after remove = (%v, %v), want (false, nil)", vni, exists, err)
		}
	})
}

func TestLinuxFabricVXLANSelfHeal(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		const vni = 1000
		bringLoopbackUp(t)

		// Create the N1b loopback-bound VTEP (SrcAddr 127.0.0.1).
		if err := f.EnsureVXLAN(vxlanLoopbackConfig()); err != nil {
			t.Fatalf("EnsureVXLAN(loopback) = %v", err)
		}
		// Re-ensure with a different source address: an immutable-attr drift the
		// fabric must repair by delete-recreate (the N1b->otwg0 rebind shape).
		rebind := VXLANConfig{VNI: vni, Local: netip.MustParseAddr("127.0.0.2"), Port: 4789, MTU: 1390}
		if err := f.EnsureVXLAN(rebind); err != nil {
			t.Fatalf("EnsureVXLAN(rebind) = %v", err)
		}
		link, err := netlink.LinkByName("otvx1000")
		if err != nil {
			t.Fatalf("LinkByName(otvx1000) = %v", err)
		}
		vx := link.(*netlink.Vxlan)
		if !vx.SrcAddr.Equal(net.ParseIP("127.0.0.2")) {
			t.Errorf("SrcAddr = %v, want 127.0.0.2 (VTEP not recreated on drift)", vx.SrcAddr)
		}

		// Port drift also forces a recreate (immutable on a live link).
		portDrift := VXLANConfig{VNI: vni, Local: netip.MustParseAddr("127.0.0.2"), Port: 4790, MTU: 1390}
		if err := f.EnsureVXLAN(portDrift); err != nil {
			t.Fatalf("EnsureVXLAN(port drift) = %v", err)
		}
		link, err = netlink.LinkByName("otvx1000")
		if err != nil {
			t.Fatalf("LinkByName after port drift = %v", err)
		}
		if vx := link.(*netlink.Vxlan); vx.Port != 4790 {
			t.Errorf("Port = %d, want 4790 (VTEP not recreated on port drift)", vx.Port)
		}
	})
}

// TestLinuxFabricVXLANRecoverFromDownAfterEnslave reconstructs the exact crash
// window the d0f8f15 fix heals: a VTEP enslaved into a bridge but left
// admin-down (a crash between LinkSetMaster and LinkSetUp). The reconciler is
// retry-forever, so the next EnsureVXLAN tick must bring the port back up while
// keeping it enslaved. The partial state is forged out of band with
// LinkSetDown after a clean ensure.
func TestLinuxFabricVXLANRecoverFromDownAfterEnslave(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		bringLoopbackUp(t)
		if err := f.EnsureBridge("otb1000", 1390); err != nil {
			t.Fatalf("EnsureBridge = %v", err)
		}
		cfg := VXLANConfig{VNI: 1000, Local: netip.MustParseAddr("127.0.0.1"), Port: 4789, MTU: 1390, Master: "otb1000"}
		if err := f.EnsureVXLAN(cfg); err != nil {
			t.Fatalf("EnsureVXLAN(master) = %v", err)
		}

		// Forge the partial crash state: enslaved but admin-down.
		link, err := netlink.LinkByName("otvx1000")
		if err != nil {
			t.Fatalf("LinkByName(otvx1000) = %v", err)
		}
		if err := netlink.LinkSetDown(link); err != nil {
			t.Fatalf("LinkSetDown(otvx1000) = %v", err)
		}

		// The next reconciler tick must self-heal: up again, still enslaved.
		if err := f.EnsureVXLAN(cfg); err != nil {
			t.Fatalf("EnsureVXLAN re-ensure after down = %v", err)
		}
		link, err = netlink.LinkByName("otvx1000")
		if err != nil {
			t.Fatalf("LinkByName(otvx1000) after re-ensure = %v", err)
		}
		if link.Attrs().Flags&net.FlagUp == 0 {
			t.Errorf("VTEP flags = %v after re-ensure, want admin-up", link.Attrs().Flags)
		}
		if link.Attrs().MasterIndex == 0 {
			t.Errorf("VTEP MasterIndex = 0 after re-ensure, want it still enslaved")
		}
	})
}

// TestLinuxFabricVXLANRecoverFromMTUDrift forges an out-of-band MTU change on a
// live VTEP and asserts the next EnsureVXLAN reapplies the configured MTU in
// place (no delete-recreate: MTU is mutable, so SrcAddr/Port/VNI are
// untouched).
func TestLinuxFabricVXLANRecoverFromMTUDrift(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		bringLoopbackUp(t)
		cfg := vxlanLoopbackConfig() // MTU 1390
		if err := f.EnsureVXLAN(cfg); err != nil {
			t.Fatalf("EnsureVXLAN = %v", err)
		}

		// Drift the MTU out of band.
		link, err := netlink.LinkByName("otvx1000")
		if err != nil {
			t.Fatalf("LinkByName(otvx1000) = %v", err)
		}
		if err := netlink.LinkSetMTU(link, 9000); err != nil {
			t.Fatalf("LinkSetMTU(otvx1000, 9000) = %v", err)
		}

		// The next tick must correct it back to 1390.
		if err := f.EnsureVXLAN(cfg); err != nil {
			t.Fatalf("EnsureVXLAN re-ensure after mtu drift = %v", err)
		}
		link, err = netlink.LinkByName("otvx1000")
		if err != nil {
			t.Fatalf("LinkByName(otvx1000) after re-ensure = %v", err)
		}
		if mtu := link.Attrs().MTU; mtu != 1390 {
			t.Errorf("MTU = %d after re-ensure, want 1390 (drift corrected in place)", mtu)
		}
	})
}

func TestLinuxFabricVXLANEnslave(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		bringLoopbackUp(t)
		if err := f.EnsureBridge("otb1000", 1390); err != nil {
			t.Fatalf("EnsureBridge = %v", err)
		}
		cfg := VXLANConfig{VNI: 1000, Local: netip.MustParseAddr("127.0.0.1"), Port: 4789, MTU: 1390, Master: "otb1000"}
		if err := f.EnsureVXLAN(cfg); err != nil {
			t.Fatalf("EnsureVXLAN(master) = %v", err)
		}
		br, err := netlink.LinkByName("otb1000")
		if err != nil {
			t.Fatalf("LinkByName(otb1000) = %v", err)
		}
		vtep, err := netlink.LinkByName("otvx1000")
		if err != nil {
			t.Fatalf("LinkByName(otvx1000) = %v", err)
		}
		if got, want := vtep.Attrs().MasterIndex, br.Attrs().Index; got != want {
			t.Errorf("VTEP MasterIndex = %d, want %d (bridge index)", got, want)
		}
		// The VTEP must be admin-up AFTER enslavement: EnsureVXLAN sets it up
		// last so the enslave-time carrier/operstate reset cannot leave an
		// enslaved-but-down port (regression guard for the up-before-enslave bug).
		if vtep.Attrs().Flags&net.FlagUp == 0 {
			t.Errorf("enslaved VTEP flags = %v, want admin-up", vtep.Attrs().Flags)
		}
	})
}

func TestLinuxFabricVXLANFDB(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		const vni = 1000
		bringLoopbackUp(t)
		if err := f.EnsureVXLAN(vxlanLoopbackConfig()); err != nil {
			t.Fatalf("EnsureVXLAN = %v", err)
		}

		mac, err := net.ParseMAC("52:54:00:aa:bb:cc")
		if err != nil {
			t.Fatalf("ParseMAC = %v", err)
		}
		entry := FDBEntry{MAC: mac, Dst: netip.MustParseAddr("127.0.0.1")}

		list, err := f.FDBList(vni)
		if err != nil {
			t.Fatalf("FDBList before append = %v", err)
		}
		if len(list) != 0 {
			t.Fatalf("FDBList before append = %v, want empty", list)
		}

		if err := f.FDBAppend(vni, entry); err != nil {
			t.Fatalf("FDBAppend = %v", err)
		}
		// Idempotent: a second identical append is a no-op.
		if err := f.FDBAppend(vni, entry); err != nil {
			t.Fatalf("FDBAppend second call = %v", err)
		}

		list, err = f.FDBList(vni)
		if err != nil {
			t.Fatalf("FDBList after append = %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("FDBList after append = %v, want exactly 1 entry", list)
		}
		if list[0].MAC.String() != mac.String() {
			t.Errorf("entry MAC = %v, want %v", list[0].MAC, mac)
		}
		if list[0].Dst != netip.MustParseAddr("127.0.0.1") {
			t.Errorf("entry Dst = %v, want 127.0.0.1", list[0].Dst)
		}

		if err := f.FDBDelete(vni, entry); err != nil {
			t.Fatalf("FDBDelete = %v", err)
		}
		// Idempotent: deleting an absent entry is a no-op.
		if err := f.FDBDelete(vni, entry); err != nil {
			t.Fatalf("FDBDelete on absent = %v", err)
		}
		list, err = f.FDBList(vni)
		if err != nil {
			t.Fatalf("FDBList after delete = %v", err)
		}
		if len(list) != 0 {
			t.Errorf("FDBList after delete = %v, want empty", list)
		}
	})
}

func TestLinuxFabricFDBListIncludesZeroMAC(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		const vni = 1000
		bringLoopbackUp(t)
		if err := f.EnsureVXLAN(vxlanLoopbackConfig()); err != nil {
			t.Fatalf("EnsureVXLAN = %v", err)
		}
		zero := FDBEntry{MAC: net.HardwareAddr{0, 0, 0, 0, 0, 0}, Dst: netip.MustParseAddr("127.0.0.1")}
		unicast := FDBEntry{MAC: net.HardwareAddr{0x52, 0x54, 0x00, 0xaa, 0xbb, 0xcc}, Dst: netip.MustParseAddr("127.0.0.1")}
		if err := f.FDBAppend(vni, zero); err != nil {
			t.Fatalf("FDBAppend(zero) = %v", err)
		}
		if err := f.FDBAppend(vni, unicast); err != nil {
			t.Fatalf("FDBAppend(unicast) = %v", err)
		}
		list, err := f.FDBList(vni)
		if err != nil {
			t.Fatalf("FDBList = %v", err)
		}
		var sawZero, sawUnicast bool
		for _, e := range list {
			if e.MAC.String() == zero.MAC.String() {
				sawZero = true
			}
			if e.MAC.String() == unicast.MAC.String() {
				sawUnicast = true
			}
		}
		if !sawZero {
			t.Errorf("FDBList omitted the zero-MAC flood entry; want it present for prune")
		}
		if !sawUnicast {
			t.Errorf("FDBList missing the unicast entry")
		}
		// Prune the zero-MAC entry and confirm it is gone.
		if err := f.FDBDelete(vni, zero); err != nil {
			t.Fatalf("FDBDelete(zero) = %v", err)
		}
		list, err = f.FDBList(vni)
		if err != nil {
			t.Fatalf("FDBList after delete = %v", err)
		}
		for _, e := range list {
			if e.MAC.String() == zero.MAC.String() {
				t.Errorf("zero-MAC entry still present after FDBDelete")
			}
		}
	})
}

// TestLinuxFabricVXLANNegativePaths covers the error branches: a non-vxlan
// link squatting the otvx<vni> name must not be adopted by EnsureVXLAN or
// programmed by FDBAppend, VXLANExists reports false for it, and FDB ops on a
// fully absent VTEP error rather than panic. Mirrors the bridge/tap side's
// TestLinuxFabricCreateTapOverBridge negative coverage.
func TestLinuxFabricVXLANNegativePaths(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		bringLoopbackUp(t)

		const vni = 1000
		mac, err := net.ParseMAC("52:54:00:de:ad:be")
		if err != nil {
			t.Fatalf("ParseMAC = %v", err)
		}
		entry := FDBEntry{MAC: mac, Dst: netip.MustParseAddr("127.0.0.1")}

		// A non-vxlan link occupying the otvx<vni> name must not be adopted.
		if err := f.EnsureBridge("otvx1000", 1500); err != nil {
			t.Fatalf("EnsureBridge(otvx1000) = %v", err)
		}
		if err := f.EnsureVXLAN(vxlanLoopbackConfig()); err == nil {
			t.Errorf("EnsureVXLAN over a non-vxlan link = nil, want error")
		}
		if exists, err := f.VXLANExists(vni); err != nil || exists {
			t.Errorf("VXLANExists over a non-vxlan link = (%v, %v), want (false, nil)", exists, err)
		}
		if err := f.FDBAppend(vni, entry); err == nil {
			t.Errorf("FDBAppend over a non-vxlan link = nil, want error")
		}

		// FDB ops on a fully absent VTEP must error, not panic.
		const absent = 4242
		if err := f.FDBAppend(absent, entry); err == nil {
			t.Errorf("FDBAppend on absent vtep = nil, want error")
		}
		if _, err := f.FDBList(absent); err == nil {
			t.Errorf("FDBList on absent vtep = nil, want error")
		}
		if err := f.FDBDelete(absent, entry); err == nil {
			t.Errorf("FDBDelete on absent vtep = nil, want error")
		}
	})
}

// wgTestConfig is the shared N2a scaffold WireGuard config: otwg0 with a fresh
// private key, the default listen port, an overlay /24 address, and the WG MTU.
func wgTestConfig(t *testing.T) WGConfig {
	t.Helper()
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey = %v", err)
	}
	return WGConfig{
		Name:       "otwg0",
		PrivateKey: key,
		ListenPort: 51820,
		Address:    netip.MustParsePrefix("10.42.0.1/24"),
		MTU:        1420,
	}
}

func TestLinuxFabricWireGuardLifecycle(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		cfg := wgTestConfig(t)

		exists, err := f.WireGuardExists(cfg.Name)
		if err != nil {
			t.Fatalf("WireGuardExists(%s) = %v", cfg.Name, err)
		}
		if exists {
			t.Fatalf("WireGuardExists(%s) = true before create, want false", cfg.Name)
		}

		if err := f.EnsureWireGuard(cfg); err != nil {
			t.Fatalf("EnsureWireGuard(%s) = %v", cfg.Name, err)
		}
		if err := f.EnsureWireGuard(cfg); err != nil {
			t.Fatalf("EnsureWireGuard second call = %v", err)
		}

		exists, err = f.WireGuardExists(cfg.Name)
		if err != nil || !exists {
			t.Fatalf("WireGuardExists(%s) after create = (%v, %v), want (true, nil)", cfg.Name, exists, err)
		}

		link, err := netlink.LinkByName(cfg.Name)
		if err != nil {
			t.Fatalf("LinkByName(%s) = %v", cfg.Name, err)
		}
		if _, ok := link.(*netlink.Wireguard); !ok {
			t.Fatalf("%s is %T, want *netlink.Wireguard", cfg.Name, link)
		}
		if mtu := link.Attrs().MTU; mtu != 1420 {
			t.Errorf("MTU = %d, want 1420", mtu)
		}
		if link.Attrs().Flags&net.FlagUp == 0 {
			t.Errorf("%s is not up", cfg.Name)
		}

		addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			t.Fatalf("AddrList(%s) = %v", cfg.Name, err)
		}
		foundAddr := false
		for _, a := range addrs {
			if a.IPNet != nil && a.IPNet.String() == "10.42.0.1/24" {
				foundAddr = true
			}
		}
		if !foundAddr {
			t.Errorf("address 10.42.0.1/24 not found on %s, got %v", cfg.Name, addrs)
		}

		c, err := wgctrl.New()
		if err != nil {
			t.Fatalf("wgctrl.New = %v", err)
		}
		defer c.Close()
		dev, err := c.Device(cfg.Name)
		if err != nil {
			t.Fatalf("Device(%s) = %v", cfg.Name, err)
		}
		if dev.PrivateKey != cfg.PrivateKey {
			t.Errorf("PrivateKey did not round-trip")
		}
		if dev.ListenPort != cfg.ListenPort {
			t.Errorf("ListenPort = %d, want %d", dev.ListenPort, cfg.ListenPort)
		}

		if err := f.RemoveWireGuard(cfg.Name); err != nil {
			t.Fatalf("RemoveWireGuard(%s) = %v", cfg.Name, err)
		}
		if err := f.RemoveWireGuard(cfg.Name); err != nil {
			t.Fatalf("RemoveWireGuard(%s) on absent = %v", cfg.Name, err)
		}
		exists, err = f.WireGuardExists(cfg.Name)
		if err != nil || exists {
			t.Fatalf("WireGuardExists(%s) after remove = (%v, %v), want (false, nil)", cfg.Name, exists, err)
		}
	})
}

// TestLinuxFabricWireGuardRecoverFromDown forges an admin-down WireGuard link
// out of band (a crash after LinkAdd/configure but before LinkSetUp) and
// asserts the next EnsureWireGuard tick brings it back up with the overlay
// address still present. The reconciler is retry-forever, so a half-built
// device must self-heal.
func TestLinuxFabricWireGuardRecoverFromDown(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		cfg := wgTestConfig(t)
		if err := f.EnsureWireGuard(cfg); err != nil {
			t.Fatalf("EnsureWireGuard = %v", err)
		}

		link, err := netlink.LinkByName(cfg.Name)
		if err != nil {
			t.Fatalf("LinkByName(%s) = %v", cfg.Name, err)
		}
		if err := netlink.LinkSetDown(link); err != nil {
			t.Fatalf("LinkSetDown(%s) = %v", cfg.Name, err)
		}

		if err := f.EnsureWireGuard(cfg); err != nil {
			t.Fatalf("EnsureWireGuard re-ensure after down = %v", err)
		}
		link, err = netlink.LinkByName(cfg.Name)
		if err != nil {
			t.Fatalf("LinkByName(%s) after re-ensure = %v", cfg.Name, err)
		}
		if link.Attrs().Flags&net.FlagUp == 0 {
			t.Errorf("%s flags = %v after re-ensure, want admin-up", cfg.Name, link.Attrs().Flags)
		}

		addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			t.Fatalf("AddrList(%s) = %v", cfg.Name, err)
		}
		foundAddr := false
		for _, a := range addrs {
			if a.IPNet != nil && a.IPNet.String() == "10.42.0.1/24" {
				foundAddr = true
			}
		}
		if !foundAddr {
			t.Errorf("address 10.42.0.1/24 not found on %s after re-ensure, got %v", cfg.Name, addrs)
		}
	})
}

func TestLinuxFabricWireGuardPeers(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		cfg := wgTestConfig(t)
		if err := f.EnsureWireGuard(cfg); err != nil {
			t.Fatalf("EnsureWireGuard = %v", err)
		}

		peerKey, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			t.Fatalf("GeneratePrivateKey(peer) = %v", err)
		}
		peer := WGPeer{
			PublicKey:           peerKey.PublicKey(),
			Endpoint:            &net.UDPAddr{IP: net.ParseIP("203.0.113.5"), Port: 51820},
			AllowedIPs:          []netip.Prefix{netip.MustParsePrefix("10.42.1.0/24")},
			PersistentKeepalive: 25 * time.Second,
		}

		peers, err := f.WireGuardPeers(cfg.Name)
		if err != nil {
			t.Fatalf("WireGuardPeers before set = %v", err)
		}
		if len(peers) != 0 {
			t.Fatalf("WireGuardPeers before set = %v, want empty", peers)
		}

		if err := f.SetWireGuardPeers(cfg.Name, []WGPeer{peer}); err != nil {
			t.Fatalf("SetWireGuardPeers = %v", err)
		}
		peers, err = f.WireGuardPeers(cfg.Name)
		if err != nil {
			t.Fatalf("WireGuardPeers after set = %v", err)
		}
		if len(peers) != 1 {
			t.Fatalf("WireGuardPeers = %v, want exactly 1", peers)
		}
		if peers[0].PublicKey != peer.PublicKey {
			t.Errorf("peer pubkey did not round-trip")
		}
		if len(peers[0].AllowedIPs) != 1 || peers[0].AllowedIPs[0] != netip.MustParsePrefix("10.42.1.0/24") {
			t.Errorf("AllowedIPs = %v, want [10.42.1.0/24]", peers[0].AllowedIPs)
		}
		if peers[0].Endpoint == nil || peers[0].Endpoint.String() != "203.0.113.5:51820" {
			t.Errorf("Endpoint = %v, want 203.0.113.5:51820", peers[0].Endpoint)
		}
		if peers[0].PersistentKeepalive != 25*time.Second {
			t.Errorf("PersistentKeepalive = %v, want 25s", peers[0].PersistentKeepalive)
		}

		key2, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			t.Fatalf("GeneratePrivateKey(peer2) = %v", err)
		}
		peer2 := WGPeer{
			PublicKey:  key2.PublicKey(),
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.42.2.0/24")},
		}
		if err := f.SetWireGuardPeers(cfg.Name, []WGPeer{peer2}); err != nil {
			t.Fatalf("SetWireGuardPeers replace = %v", err)
		}
		peers, err = f.WireGuardPeers(cfg.Name)
		if err != nil {
			t.Fatalf("WireGuardPeers after replace = %v", err)
		}
		if len(peers) != 1 || peers[0].PublicKey != peer2.PublicKey {
			t.Fatalf("after replace = %v, want only peer2", peers)
		}

		if err := f.SetWireGuardPeers(cfg.Name, nil); err != nil {
			t.Fatalf("SetWireGuardPeers(nil) = %v", err)
		}
		peers, err = f.WireGuardPeers(cfg.Name)
		if err != nil {
			t.Fatalf("WireGuardPeers after clear = %v", err)
		}
		if len(peers) != 0 {
			t.Errorf("after clear = %v, want empty", peers)
		}
	})
}

// TestLinuxFabricWireGuardPeerHandshakes asserts WireGuardPeerHandshakes
// reports the configured peer with a zero LastHandshake when no handshake has
// completed. A single-ended netns config never completes a handshake (no
// reachable peer); the real two-ended handshake is a later cross-host smoke.
func TestLinuxFabricWireGuardPeerHandshakes(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		cfg := wgTestConfig(t)
		if err := f.EnsureWireGuard(cfg); err != nil {
			t.Fatalf("EnsureWireGuard = %v", err)
		}

		peerKey, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			t.Fatalf("GeneratePrivateKey(peer) = %v", err)
		}
		if err := f.SetWireGuardPeers(cfg.Name, []WGPeer{{
			PublicKey:  peerKey.PublicKey(),
			AllowedIPs: []netip.Prefix{netip.MustParsePrefix("10.42.0.2/32")},
		}}); err != nil {
			t.Fatalf("SetWireGuardPeers = %v", err)
		}

		got, err := f.WireGuardPeerHandshakes(cfg.Name)
		if err != nil {
			t.Fatalf("WireGuardPeerHandshakes = %v", err)
		}
		if len(got) != 1 || got[0].PublicKey != peerKey.PublicKey() {
			t.Fatalf("WireGuardPeerHandshakes = %v, want one peer %v", got, peerKey.PublicKey())
		}
		if !got[0].LastHandshake.IsZero() {
			t.Errorf("LastHandshake = %v, want zero (no completed handshake single-ended)", got[0].LastHandshake)
		}
	})
}

func TestLinuxFabricWireGuardNegativePaths(t *testing.T) {
	withNetNS(t, func() {
		f := New()

		dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "otwg0"}}
		if err := netlink.LinkAdd(dummy); err != nil {
			t.Fatalf("LinkAdd(dummy otwg0) = %v", err)
		}
		exists, err := f.WireGuardExists("otwg0")
		if err != nil {
			t.Fatalf("WireGuardExists(dummy otwg0) = %v", err)
		}
		if exists {
			t.Errorf("WireGuardExists on a dummy link = true, want false")
		}
		if err := f.EnsureWireGuard(wgTestConfig(t)); err == nil {
			t.Errorf("EnsureWireGuard over a dummy link = nil, want type-collision error")
		}
		if err := netlink.LinkDel(dummy); err != nil {
			t.Fatalf("LinkDel(dummy) = %v", err)
		}

		if err := f.SetWireGuardPeers("otwg0", nil); err == nil {
			t.Errorf("SetWireGuardPeers on absent device = nil, want error")
		}
		if _, err := f.WireGuardPeers("otwg0"); err == nil {
			t.Errorf("WireGuardPeers on absent device = nil, want error")
		}
	})
}

func TestLinuxFabricWireGuardConcurrent(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		cfg := wgTestConfig(t)
		if err := f.EnsureWireGuard(cfg); err != nil {
			t.Fatalf("EnsureWireGuard = %v", err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = f.EnsureWireGuard(cfg)
				_ = f.SetWireGuardPeers(cfg.Name, nil)
				_, _ = f.WireGuardPeers(cfg.Name)
			}()
		}
		wg.Wait()
		exists, err := f.WireGuardExists(cfg.Name)
		if err != nil || !exists {
			t.Fatalf("WireGuardExists after contention = (%v, %v), want (true, nil)", exists, err)
		}
	})
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

// TestLinuxFabricBridgeRecoverFromDown forges an admin-down bridge out of band
// (a crash after LinkAdd but before LinkSetUp) and asserts the next
// EnsureBridge tick brings it back up. The reconciler is retry-forever, so a
// half-built bridge must self-heal.
func TestLinuxFabricBridgeRecoverFromDown(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		const name = "ot-br-down0"
		if err := f.EnsureBridge(name, 1450); err != nil {
			t.Fatalf("EnsureBridge(%q, 1450) = %v", name, err)
		}

		link, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatalf("LinkByName(%q) = %v", name, err)
		}
		if err := netlink.LinkSetDown(link); err != nil {
			t.Fatalf("LinkSetDown(%q) = %v", name, err)
		}

		if err := f.EnsureBridge(name, 1450); err != nil {
			t.Fatalf("EnsureBridge(%q) re-ensure after down = %v", name, err)
		}
		link, err = netlink.LinkByName(name)
		if err != nil {
			t.Fatalf("LinkByName(%q) after re-ensure = %v", name, err)
		}
		if link.Attrs().Flags&net.FlagUp == 0 {
			t.Errorf("bridge %q flags = %v after re-ensure, want admin-up", name, link.Attrs().Flags)
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
// chain carry a marker for subnet, across every egress iface.
func countMasqRules(t *testing.T, subnet netip.Prefix) int {
	t.Helper()
	c := &nftables.Conn{}
	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: natTableName}
	chain := &nftables.Chain{Name: natChainName, Table: table}
	rules, err := c.GetRules(table, chain)
	if err != nil {
		t.Fatalf("GetRules(%s/%s) = %v", natTableName, natChainName, err)
	}
	prefix := masqSubnetPrefix(subnet)
	n := 0
	for _, r := range rules {
		if bytes.HasPrefix(r.UserData, prefix) {
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
			requireNetfabric(t, "EnsureMasquerade(%s, lo) = %v (nftables unavailable in netns?)", subnet, err)
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
			requireNetfabric(t, "ListTables() = %v (nftables unavailable in netns?)", err)
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

// TestLinuxFabricCreateTapOverBridge asserts that CreateTap refuses to
// adopt a link of a different type: pointing it at an existing bridge
// returns the type-assert error and leaves the bridge in place.
func TestLinuxFabricCreateTapOverBridge(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		const name = "otbr0"

		if err := f.EnsureBridge(name, 1500); err != nil {
			t.Fatalf("EnsureBridge(%q, 1500) = %v", name, err)
		}

		err := f.CreateTap(name, 1500)
		if err == nil {
			t.Fatalf("CreateTap(%q) over a bridge = nil, want type-assert error", name)
		}
		if !strings.Contains(err.Error(), "not a tap") {
			t.Errorf("CreateTap(%q) error = %v, want it to mention \"not a tap\"", name, err)
		}

		// The bridge must be untouched - CreateTap must not delete or
		// convert it.
		link, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatalf("LinkByName(%q) after failed CreateTap = %v", name, err)
		}
		if _, ok := link.(*netlink.Bridge); !ok {
			t.Errorf("link %q is %T after failed CreateTap, want *netlink.Bridge", name, link)
		}
	})
}

// TestLinuxFabricListTaps asserts ListTaps returns exactly the
// ot-prefixed tap devices, sorted, ignoring bridges and non-ot taps.
func TestLinuxFabricListTaps(t *testing.T) {
	withNetNS(t, func() {
		f := New()

		if err := f.CreateTap("otbeef0", 1500); err != nil {
			t.Fatalf("CreateTap(otbeef0) = %v", err)
		}
		if err := f.CreateTap("otaaaa0", 1500); err != nil {
			t.Fatalf("CreateTap(otaaaa0) = %v", err)
		}
		if err := f.EnsureBridge("otbridge0", 1500); err != nil {
			t.Fatalf("EnsureBridge(otbridge0) = %v", err)
		}
		// A tap without the ot prefix must be ignored.
		if err := f.CreateTap("xtap0", 1500); err != nil {
			t.Fatalf("CreateTap(xtap0) = %v", err)
		}

		got, err := f.ListTaps()
		if err != nil {
			t.Fatalf("ListTaps() = %v", err)
		}
		want := []string{"otaaaa0", "otbeef0"}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("ListTaps() mismatch (-want +got):\n%s", diff)
		}
	})
}

// TestLinuxFabricConcurrentMutators hammers the bridge / tap / NAT
// mutators of one shared fabric from several goroutines at once, the way
// the reconciler goroutine and the VM-manager handler goroutines share a
// single instance in production. It is a smoke for the fabric-level mutex
// (FIX 4): the assertion is that no call deadlocks and the process does
// not crash on a concurrent netlink / nftables sequence. Individual op
// errors are tolerated (a tap attach can lose a race with a bridge remove);
// a hang or panic is the failure this guards against.
func TestLinuxFabricConcurrentMutators(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		const bridge = "ot-br-conc0"
		const vni = 1000
		subnet := netip.MustParsePrefix("10.88.0.0/24")
		gw := netip.MustParsePrefix("10.88.0.1/24")
		bringLoopbackUp(t)

		if err := f.EnsureBridge(bridge, 1500); err != nil {
			t.Fatalf("EnsureBridge(%q) = %v", bridge, err)
		}

		const workers = 8
		var wg sync.WaitGroup
		wg.Add(workers)
		for i := range workers {
			go func(n int) {
				defer wg.Done()
				tap := "otconc" + string(rune('a'+n))
				mac := net.HardwareAddr{0x52, 0x54, 0x00, 0x00, 0x00, byte(n)}
				entry := FDBEntry{MAC: mac, Dst: netip.MustParseAddr("127.0.0.1")}
				// Each goroutine drives the full mutator surface, including the
				// VXLAN VTEP + FDB mutators that share the same f.mu and that a
				// concurrent RemoveVXLAN could otherwise race against vxlanIndex.
				// Errors are intentionally ignored: the point is that the mutex
				// keeps these from corrupting shared kernel state or deadlocking.
				_ = f.EnsureBridge(bridge, 1500)
				_ = f.CreateTap(tap, 1500)
				_ = f.AttachTap(tap, bridge)
				_ = f.EnsureGatewayAddr(bridge, gw)
				_ = f.EnsureMasquerade(subnet, "lo")
				_ = f.EnsureVXLAN(vxlanLoopbackConfig())
				_ = f.FDBAppend(vni, entry)
				_ = f.FDBDelete(vni, entry)
				_ = f.RemoveVXLAN(vni)
				_ = f.RemoveMasquerade(subnet)
				_ = f.RemoveGatewayAddr(bridge, gw)
				_ = f.DeleteTap(tap)
			}(i)
		}
		wg.Wait()

		// The shared bridge must still be removable after the storm.
		if err := f.RemoveBridge(bridge); err != nil {
			t.Errorf("RemoveBridge(%q) after concurrent storm = %v", bridge, err)
		}
	})
}

// TestLinuxFabricMasqueradeMultiIface asserts that masquerade rules are
// keyed on subnet+iface: ensuring the same subnet via two egress ifaces
// installs two distinct rules, and a single RemoveMasquerade(subnet)
// removes both.
func TestLinuxFabricMasqueradeMultiIface(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		subnet := netip.MustParsePrefix("10.99.0.0/24")

		// A second usable egress iface besides lo: a dummy link.
		const dummyName = "otdummy0"
		if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: dummyName}}); err != nil {
			t.Fatalf("LinkAdd(dummy %q) = %v", dummyName, err)
		}

		if err := f.EnsureMasquerade(subnet, "lo"); err != nil {
			requireNetfabric(t, "EnsureMasquerade(%s, lo) = %v (nftables unavailable in netns?)", subnet, err)
		}
		if n := countMasqRules(t, subnet); n != 1 {
			t.Fatalf("masquerade rule count = %d after first iface, want 1", n)
		}

		if err := f.EnsureMasquerade(subnet, dummyName); err != nil {
			t.Fatalf("EnsureMasquerade(%s, %s) = %v", subnet, dummyName, err)
		}
		if n := countMasqRules(t, subnet); n != 2 {
			t.Fatalf("masquerade rule count = %d after second iface, want 2", n)
		}

		// Idempotent per (subnet, iface): re-ensuring lo adds no rule.
		if err := f.EnsureMasquerade(subnet, "lo"); err != nil {
			t.Fatalf("EnsureMasquerade(%s, lo) repeat = %v", subnet, err)
		}
		if n := countMasqRules(t, subnet); n != 2 {
			t.Fatalf("masquerade rule count = %d after lo repeat, want 2", n)
		}

		// A single removal clears every iface's rule for the subnet.
		if err := f.RemoveMasquerade(subnet); err != nil {
			t.Fatalf("RemoveMasquerade(%s) = %v", subnet, err)
		}
		if n := countMasqRules(t, subnet); n != 0 {
			t.Fatalf("masquerade rule count = %d after remove, want 0", n)
		}
	})
}

func TestLinuxFabricLinkState(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		// Absent link -> zero LinkState, nil error.
		st, err := f.LinkState("otb1000")
		if err != nil || st.Up {
			t.Fatalf("LinkState(absent) = (%+v, %v), want (zero, nil)", st, err)
		}
		// Create a bridge with a known MTU + address and read it back.
		if err := f.EnsureBridge("otb1000", 1390); err != nil {
			t.Fatalf("EnsureBridge = %v", err)
		}
		if err := f.EnsureGatewayAddr("otb1000", netip.MustParsePrefix("10.52.0.1/24")); err != nil {
			t.Fatalf("EnsureGatewayAddr = %v", err)
		}
		st, err = f.LinkState("otb1000")
		if err != nil {
			t.Fatalf("LinkState(otb1000) = %v", err)
		}
		if !st.Up {
			t.Errorf("Up = false, want true")
		}
		if st.MTU != 1390 {
			t.Errorf("MTU = %d, want 1390", st.MTU)
		}
		found := false
		for _, p := range st.Addrs {
			if p == netip.MustParsePrefix("10.52.0.1/24") {
				found = true
			}
		}
		if !found {
			t.Errorf("Addrs = %v, want to contain 10.52.0.1/24", st.Addrs)
		}
	})
}

func TestLinuxFabricEnableIPForwarding(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		if err := f.EnableIPForwarding(); err != nil {
			requireNetfabric(t, "EnableIPForwarding() = %v", err)
		}
		got, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
		if err != nil {
			t.Fatalf("read ip_forward = %v", err)
		}
		if strings.TrimSpace(string(got)) != "1" {
			t.Errorf("ip_forward = %q, want 1", strings.TrimSpace(string(got)))
		}
		// Idempotent: a second call must succeed and leave it enabled.
		if err := f.EnableIPForwarding(); err != nil {
			t.Errorf("EnableIPForwarding() second call = %v", err)
		}
	})
}

func TestLinuxFabricAnycastGateway(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		const bridge = "ot-br-any0"
		mac := GatewayMAC(0x001000) // 4096
		if err := f.EnsureBridge(bridge, 1500); err != nil {
			t.Fatalf("EnsureBridge = %v", err)
		}
		if err := f.EnsureAnycastGateway(bridge, OverlayGatewayAddr, mac); err != nil {
			requireNetfabric(t, "EnsureAnycastGateway = %v", err)
		}

		link, err := netlink.LinkByName(bridge)
		if err != nil {
			t.Fatalf("LinkByName = %v", err)
		}
		if got := link.Attrs().HardwareAddr.String(); got != mac.String() {
			t.Errorf("bridge MAC = %v, want %v", got, mac.String())
		}
		addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			t.Fatalf("AddrList = %v", err)
		}
		want := OverlayGatewayAddr.String() + "/32"
		found := false
		for _, a := range addrs {
			if a.IPNet != nil && a.IPNet.String() == want {
				found = true
			}
		}
		if !found {
			t.Errorf("addr %s not present on %q", want, bridge)
		}

		// Idempotent re-assert.
		if err := f.EnsureAnycastGateway(bridge, OverlayGatewayAddr, mac); err != nil {
			t.Errorf("EnsureAnycastGateway second call = %v", err)
		}

		// Removal drops the address; idempotent on absent.
		if err := f.RemoveAnycastGateway(bridge, OverlayGatewayAddr); err != nil {
			t.Fatalf("RemoveAnycastGateway = %v", err)
		}
		addrs, _ = netlink.AddrList(link, netlink.FAMILY_V4)
		for _, a := range addrs {
			if a.IPNet != nil && a.IPNet.String() == want {
				t.Errorf("addr %s still present after RemoveAnycastGateway", want)
			}
		}
		if err := f.RemoveAnycastGateway(bridge, OverlayGatewayAddr); err != nil {
			t.Errorf("RemoveAnycastGateway on absent = %v", err)
		}
	})
}
