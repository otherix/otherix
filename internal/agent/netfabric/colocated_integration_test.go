// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux && integration
// +build linux,integration

// This file is package netfabric_test (an external test package) on purpose:
// it drives the real reconciler.Networks over the real linuxFabric, and the
// reconciler imports netfabric, so an in-package (package netfabric) test would
// be an import cycle. An external test package in the same directory is compiled
// into the same test binary that `make test-netfabric` runs, so it exercises the
// reconciler seam without the cycle. The netns scaffolding is reimplemented here
// (rather than shared) for the same reason: the in-package helpers are not
// visible across the package boundary.
package netfabric_test

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/reconciler"
)

// TestColocatedReconcilerServicesAndIngressVethOnOneBridge is the
// reconciler-level proof of gateway co-location: ONE Networks reconciler with
// hostsVMs=true, handed ONE overlay that declares BOTH the services flags
// (egress=nat, dns) AND a GatewayAddr, must materialise both planes on the same
// otvb<vni> bridge on a real Linux netns:
//
//   - the bridge carries the anycast MAC and its connected /24 route (the
//     services plane for the node's own VMs), and
//   - the ingress veth host end (otvg<vni>) carries the tenant IP + MAC, a
//     scoped /24 route, and the loose-rp_filter / arp sysctls, with its peer
//     enslaved to the bridge (the per-membership ingress plane).
//
// The lower-level coexistence test proves the netfabric primitives can share a
// bridge; this proves the single reconciler instance drives both from one
// declared network, through its real HandleHeartbeatResponse -> Run seam.
func TestColocatedReconcilerServicesAndIngressVethOnOneBridge(t *testing.T) {
	withNetNS(t, func(ns netns.NsHandle) {
		f := netfabric.New()

		const (
			vni         uint32 = 42
			bridge             = "otvb42"
			hostVeth           = "otvg42"
			peerVeth           = "otvgp42"
			subnetStr          = "10.42.0.0/24"
			tenantIP           = "10.42.0.9"
			tenantMAC          = "02:00:00:00:00:2a"
			selfOverlay        = "10.200.0.1/16"
		)

		// The overlay VTEP binds its source to this node's otwg0 overlay IP, so
		// the reconciler holds the overlay pending until otwg0 is up and carries
		// exactly the declared self_overlay_ip. Bring it up out of band.
		bringOverlayWireGuardUp(t, f, selfOverlay)

		r, err := reconciler.NewNetworks(f, nil, discardLogger(), 40*time.Millisecond, true, false)
		if err != nil {
			t.Fatalf("NewNetworks(hostsVMs=true) = %v", err)
		}

		vniI32 := int32(vni)
		self := selfOverlay
		sub := subnetStr
		resp := &heartbeat.Response{
			SelfOverlayIP: &self,
			DeclaredNetworks: []heartbeat.DeclaredNetwork{{
				ID:         "net-colocated",
				Name:       "colocated",
				Type:       "overlay",
				Managed:    true,
				Egress:     "nat",
				BridgeName: bridge,
				Mtu:        1390,
				VNI:        &vniI32,
				Subnet:     &sub,
				DNSEnabled: true,
				GatewayAddr: &heartbeat.GatewayAddr{
					IP:  tenantIP,
					MAC: tenantMAC,
				},
			}},
		}

		// Drive the real seam: publish desired state, then run the reconciler in
		// a goroutine pinned to THIS test netns (netns membership is per-thread,
		// so the Run goroutine must join the namespace the test created).
		ctx, cancel := context.WithCancel(context.Background())
		r.HandleHeartbeatResponse(ctx, resp)
		done := make(chan error, 1)
		go func() {
			runtime.LockOSThread() // deliberately never unlocked: the thread dies with the goroutine
			if err := netns.Set(ns); err != nil {
				done <- err
				return
			}
			done <- r.Run(ctx)
		}()

		// Poll from the test thread (already in this netns) until both planes
		// have converged: the bridge exists carrying the anycast MAC and the
		// ingress veth host end is present. The reconciler re-applies idempotently
		// every pass, so once converged the state is stable for the assertions.
		anycastMAC := netfabric.GatewayMAC(vni)
		converged := waitFor(5*time.Second, func() bool {
			br, err := netlink.LinkByName(bridge)
			if err != nil || br.Attrs().HardwareAddr.String() != anycastMAC.String() {
				return false
			}
			_, err = netlink.LinkByName(hostVeth)
			return err == nil
		})
		cancel()
		<-done
		if !converged {
			t.Fatalf("reconciler did not converge both planes on %s within the deadline", bridge)
		}

		// The bridge carries the anycast services-plane identity.
		br, err := netlink.LinkByName(bridge)
		if err != nil {
			t.Fatalf("bridge link: %v", err)
		}
		if got := br.Attrs().HardwareAddr.String(); got != anycastMAC.String() {
			t.Errorf("bridge MAC = %v, want anycast %v", got, anycastMAC)
		}

		// The ingress veth host end carries the tenant IP + MAC.
		hl, err := netlink.LinkByName(hostVeth)
		if err != nil {
			t.Fatalf("veth host link: %v", err)
		}
		if got := hl.Attrs().HardwareAddr.String(); got != tenantMAC {
			t.Errorf("veth host MAC = %v, want tenant %v", got, tenantMAC)
		}
		wantAddr := tenantIP + "/24"
		addrs, err := netlink.AddrList(hl, netlink.FAMILY_V4)
		if err != nil {
			t.Fatalf("veth host AddrList: %v", err)
		}
		if !hasAddr(addrs, wantAddr) {
			t.Errorf("veth host addrs = %v, want %s", addrs, wantAddr)
		}

		// The same guest /24 is connected on BOTH the bridge (services plane) and
		// the veth host end (ingress plane) - the co-location invariant.
		routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
		if err != nil {
			t.Fatalf("route list: %v", err)
		}
		var onBridge, onVeth bool
		for _, rt := range routes {
			if rt.Dst == nil || rt.Dst.String() != subnetStr {
				continue
			}
			switch rt.LinkIndex {
			case br.Attrs().Index:
				onBridge = true
			case hl.Attrs().Index:
				onVeth = true
			}
		}
		if !onBridge {
			t.Errorf("connected %s route not present on the bridge (services plane)", subnetStr)
		}
		if !onVeth {
			t.Errorf("connected %s route not present on the veth host end (ingress plane)", subnetStr)
		}

		// The host-end sysctls let an ingress reply survive the bridge's second
		// /24 (loose rp_filter) and keep the tenant IP/MAC off the bridge ARP.
		if got := readVethSysctl(t, hostVeth, "rp_filter"); got != "2" {
			t.Errorf("veth host rp_filter = %q, want 2", got)
		}
		if got := readVethSysctl(t, hostVeth, "arp_ignore"); got != "1" {
			t.Errorf("veth host arp_ignore = %q, want 1", got)
		}
		if got := readVethSysctl(t, hostVeth, "arp_announce"); got != "2" {
			t.Errorf("veth host arp_announce = %q, want 2", got)
		}

		// The veth peer is enslaved to the same bridge that carries the anycast MAC.
		peer, err := netlink.LinkByName(peerVeth)
		if err != nil {
			t.Fatalf("veth peer link: %v", err)
		}
		if peer.Attrs().MasterIndex != br.Attrs().Index {
			t.Errorf("veth peer MasterIndex = %d, want bridge index %d", peer.Attrs().MasterIndex, br.Attrs().Index)
		}
	})
}

// bringOverlayWireGuardUp brings otwg0 up carrying selfOverlay so the overlay
// reconcile passes its VTEP-source identity gate. A missing WireGuard capability
// skips (or, under OTHERIX_NETFABRIC_REQUIRE, fails) rather than masking the
// co-location assertions behind an unrelated setup error.
func bringOverlayWireGuardUp(t *testing.T, f netfabric.Fabric, selfOverlay string) {
	t.Helper()
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatalf("GeneratePrivateKey = %v", err)
	}
	if err := f.EnsureWireGuard(netfabric.WGConfig{
		Name:       "otwg0",
		PrivateKey: key,
		ListenPort: 51820,
		Address:    netip.MustParsePrefix(selfOverlay),
		MTU:        netfabric.WireGuardMTU,
	}); err != nil {
		requireDataplane(t, "EnsureWireGuard(otwg0) = %v (wireguard unavailable in netns?)", err)
	}
}

// waitFor polls cond every 20ms until it returns true or the deadline elapses,
// reporting whether it converged.
func waitFor(within time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(within)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// hasAddr reports whether addrs carries the CIDR want.
func hasAddr(addrs []netlink.Addr, want string) bool {
	for _, a := range addrs {
		if a.IPNet != nil && a.IPNet.String() == want {
			return true
		}
	}
	return false
}

// readVethSysctl reads /proc/sys/net/ipv4/conf/<iface>/<key> in the current netns.
func readVethSysctl(t *testing.T, iface, key string) string {
	t.Helper()
	b, err := os.ReadFile("/proc/sys/net/ipv4/conf/" + iface + "/" + key)
	if err != nil {
		t.Fatalf("read sysctl %s/%s: %v", iface, key, err)
	}
	return strings.TrimSpace(string(b))
}

// discardLogger returns a slog.Logger that drops all output, keeping the test
// log clean of the reconciler's best-effort WARN chatter.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// requireDataplane mirrors the in-package requireNetfabric: by default it SKIPS
// when a data-plane capability is unavailable (unprivileged laptop), but when
// OTHERIX_NETFABRIC_REQUIRE is set (the privileged CI netns job) it FAILS, so a
// regression that strips the host's data-plane capabilities cannot pass green by
// skipping. Like t.Skipf / t.Fatalf it never returns.
func requireDataplane(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("OTHERIX_NETFABRIC_REQUIRE") != "" {
		t.Fatalf("OTHERIX_NETFABRIC_REQUIRE set but a data-plane capability is unavailable: "+format, args...)
	}
	t.Skipf(format, args...)
}

// withNetNS runs fn inside a fresh, throwaway network namespace so the test never
// touches the host's real interfaces, and hands fn the namespace handle so a
// spawned reconciler goroutine can join the SAME namespace (netns membership is
// per-thread). It locks the calling goroutine to its OS thread for the duration,
// as the netns API requires. Creation failure (typically no CAP_NET_ADMIN) skips,
// or fails under OTHERIX_NETFABRIC_REQUIRE. This mirrors the in-package withNetNS,
// duplicated because that helper is not visible across the package boundary.
func withNetNS(t *testing.T, fn func(ns netns.NsHandle)) {
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
		requireDataplane(t, "create netns (needs CAP_NET_ADMIN): %v", err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			t.Errorf("close test netns: %v", err)
		}
	}()

	fn(ns)
}
