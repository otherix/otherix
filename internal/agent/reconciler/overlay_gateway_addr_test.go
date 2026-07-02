// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
)

// gatewayOverlayNet is overlayNet() with a gateway tenant IP + unicast MAC and
// the overlay subnet attached, exactly as a gateway recipient receives it from
// the CP. The subnet supplies the prefix length the tenant IP is assigned with.
func gatewayOverlayNet() heartbeat.DeclaredNetwork {
	d := overlayNet()
	d.GatewayAddr = &heartbeat.GatewayAddr{IP: "10.50.0.7", MAC: "52:54:00:ab:cd:ef"}
	subnet := "10.50.0.0/24"
	d.Subnet = &subnet
	return d
}

func readyGatewayFabric() *netfabric.FakeFabric {
	return &netfabric.FakeFabric{LinkStateResult: map[string]netfabric.LinkState{
		"otwg0": {Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.42.0.5/16")}},
	}}
}

// newGatewayRec builds a gateway-mode reconciler over the given fabric.
func newGatewayRec(t *testing.T, f netfabric.Fabric) *Networks {
	t.Helper()
	rec, err := NewGatewayNetworks(f, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewGatewayNetworks: %v", err)
	}
	return rec
}

// applyPass runs one reconcile pass over the given declared networks (empty for a
// teardown pass) with the gateway-ready overlay IP.
func (r *Networks) applyPass(t *testing.T, nets ...heartbeat.DeclaredNetwork) {
	t.Helper()
	ip := "10.42.0.5/16"
	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: nets,
		SelfOverlayIP:    &ip,
	})
	r.reconcile(context.Background())
}

// TestGatewayOverlayCreatesVeth verifies a gateway overlay (GatewayAddr set)
// materialises the ingress veth (otvg<vni> host end enslaved via otvb<vni>) with
// the tenant IP + unicast MAC, and never pins the bridge hardware address.
func TestGatewayOverlayCreatesVeth(t *testing.T) {
	f := readyGatewayFabric()
	rec := newGatewayRec(t, f)
	rec.applyPass(t, gatewayOverlayNet())

	if len(f.EnsureVethCalls) != 1 {
		t.Fatalf("EnsureVethCalls = %d, want 1", len(f.EnsureVethCalls))
	}
	got := f.EnsureVethCalls[0]
	if got.HostName != "otvg1000" || got.PeerName != "otvgp1000" || got.Bridge != "otvb1000" {
		t.Errorf("veth names = {host %q peer %q bridge %q}, want {otvg1000 otvgp1000 otvb1000}",
			got.HostName, got.PeerName, got.Bridge)
	}
	// The tenant IP must carry the overlay subnet's prefix length (/24), never a
	// /32: only the subnet prefix gives the gateway an on-link route to the
	// overlay so it reaches guest VMs instead of leaking out the host default route.
	if got.Addr.String() != "10.50.0.7/24" || got.MAC.String() != "52:54:00:ab:cd:ef" {
		t.Errorf("veth addr/mac = %s / %s, want 10.50.0.7/24 / 52:54:00:ab:cd:ef", got.Addr, got.MAC)
	}
	if got.MTU != 1390 {
		t.Errorf("veth mtu = %d, want 1390", got.MTU)
	}
	if len(f.UnicastGatewayCalls) != 0 {
		t.Errorf("UnicastGatewayCalls = %d, want 0 (bridge hwaddr must not be pinned)", len(f.UnicastGatewayCalls))
	}
}

// TestGatewayOverlayVethIdempotent verifies the veth is re-asserted on every
// reconcile pass with identical arguments, so repeated passes are safe.
func TestGatewayOverlayVethIdempotent(t *testing.T) {
	f := readyGatewayFabric()
	rec := newGatewayRec(t, f)
	rec.applyPass(t, gatewayOverlayNet())
	rec.applyPass(t, gatewayOverlayNet())

	if len(f.EnsureVethCalls) != 2 {
		t.Fatalf("EnsureVethCalls = %d, want 2 (re-asserted each pass)", len(f.EnsureVethCalls))
	}
	for i, c := range f.EnsureVethCalls {
		if c.HostName != "otvg1000" || c.Bridge != "otvb1000" ||
			c.Addr.String() != "10.50.0.7/24" || c.MAC.String() != "52:54:00:ab:cd:ef" {
			t.Errorf("call %d = %+v, want {otvg1000 otvb1000 10.50.0.7/24 52:54:00:ab:cd:ef}", i, c)
		}
	}
}

// TestPlainOverlayCreatesNoVeth verifies a normal overlay network (no gateway
// addr, as a hypervisor node receives it) materialises no veth.
func TestPlainOverlayCreatesNoVeth(t *testing.T) {
	f := readyGatewayFabric()
	rec := newGatewayRec(t, f)
	rec.applyPass(t, overlayNet())

	if len(f.EnsureVethCalls) != 0 {
		t.Errorf("EnsureVeth called for a network without a gateway addr: %+v", f.EnsureVethCalls)
	}
	if len(f.UnicastGatewayCalls) != 0 {
		t.Errorf("UnicastGatewayCalls = %d, want 0", len(f.UnicastGatewayCalls))
	}
}

// TestApplyGatewayVethNoSubnetErrors verifies a gateway addr declared without an
// overlay subnet is a hard error, not a silently-installed unroutable /32: the
// tenant IP can only be routed when its subnet prefix length is known.
func TestApplyGatewayVethNoSubnetErrors(t *testing.T) {
	f := readyGatewayFabric()
	rec := newGatewayRec(t, f)
	d := gatewayOverlayNet()
	d.Subnet = nil

	hasVeth, err := rec.applyGatewayVeth(d, 1000, false)
	if err == nil {
		t.Fatalf("applyGatewayVeth with nil subnet = nil, want error")
	}
	if hasVeth {
		t.Errorf("hasVeth = true, want false (no veth attempted without a subnet)")
	}
	if len(f.EnsureVethCalls) != 0 {
		t.Errorf("EnsureVeth called despite missing subnet: %+v", f.EnsureVethCalls)
	}
}

// TestGatewayMembershipReapRemovesVeth verifies that when the membership is
// reaped (GatewayAddr -> nil while the overlay stays declared), the
// previously-created veth is torn down.
func TestGatewayMembershipReapRemovesVeth(t *testing.T) {
	f := readyGatewayFabric()
	rec := newGatewayRec(t, f)
	rec.applyPass(t, gatewayOverlayNet()) // GatewayAddr set -> veth created
	rec.applyPass(t, overlayNet())        // same overlay id, GatewayAddr nil -> reap

	if len(f.RemoveVethCalls) != 1 || f.RemoveVethCalls[0] != "otvg1000" {
		t.Errorf("RemoveVethCalls = %v, want [otvg1000]", f.RemoveVethCalls)
	}
}

// TestNoMembershipDoesNotReap verifies an overlay that never had a membership does
// NOT call RemoveVeth every pass.
func TestNoMembershipDoesNotReap(t *testing.T) {
	f := readyGatewayFabric()
	rec := newGatewayRec(t, f)
	rec.applyPass(t, overlayNet())

	if len(f.RemoveVethCalls) != 0 {
		t.Errorf("RemoveVethCalls = %v, want none (no prior veth to reap)", f.RemoveVethCalls)
	}
}

// TestGatewayReapSkippedUnderLiveSession verifies the reap fails toward inaction
// under a live local session: a GatewayAddr -> nil drop keeps the datapath while a
// session is live and reaps it only on a later pass once the session drains.
func TestGatewayReapSkippedUnderLiveSession(t *testing.T) {
	f := readyGatewayFabric()
	rec := newGatewayRec(t, f)
	rec.applyPass(t, gatewayOverlayNet()) // veth created

	rec.AcquireSession("ov1")
	rec.applyPass(t, overlayNet()) // GatewayAddr nil, but a session is live -> keep
	if len(f.RemoveVethCalls) != 0 {
		t.Fatalf("RemoveVethCalls = %v, want none while a local session is live", f.RemoveVethCalls)
	}

	rec.ReleaseSession("ov1")
	rec.applyPass(t, overlayNet()) // session drained -> reap now
	if len(f.RemoveVethCalls) != 1 || f.RemoveVethCalls[0] != "otvg1000" {
		t.Errorf("RemoveVethCalls after drain = %v, want [otvg1000]", f.RemoveVethCalls)
	}
}

// TestGatewayOverlayTeardownRemovesVeth verifies a CP-side delete of a gateway
// overlay tears the veth down with the bridge/VTEP.
func TestGatewayOverlayTeardownRemovesVeth(t *testing.T) {
	f := readyGatewayFabric()
	rec := newGatewayRec(t, f)
	rec.applyPass(t, gatewayOverlayNet()) // create
	rec.applyPass(t)                      // empty declared set -> removeUndeclared tears down

	if len(f.RemoveVethCalls) != 1 || f.RemoveVethCalls[0] != "otvg1000" {
		t.Errorf("RemoveVethCalls on teardown = %v, want [otvg1000]", f.RemoveVethCalls)
	}
}

// TestGatewayVethTeardownAfterFailedCreate verifies a veth whose create errored
// (a partial pair may exist on the host) is still reaped when the network is
// deleted: teardown must NOT gate RemoveVeth on HasVeth, or a failed create leaks
// the pair for the rest of the process life.
func TestGatewayVethTeardownAfterFailedCreate(t *testing.T) {
	f := &netfabric.FakeFabric{
		LinkStateResult: map[string]netfabric.LinkState{
			"otwg0": {Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.42.0.5/16")}},
		},
		Errs: map[string]error{"EnsureVeth": errors.New("boom")},
	}
	rec := newGatewayRec(t, f)
	rec.applyPass(t, gatewayOverlayNet()) // EnsureVeth errors; a partial pair may exist
	rec.applyPass(t)                      // network gone -> teardown

	if len(f.RemoveVethCalls) == 0 {
		t.Errorf("RemoveVethCalls = %v, want teardown to reap despite the failed create", f.RemoveVethCalls)
	}
}
