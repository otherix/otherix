// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
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

// TestApplyOverlayPinsGatewayAddr verifies the overlay reconciler, given a
// declared network carrying a gateway addr, pins the bridge hardware address to
// the distinct unicast membership MAC and assigns the tenant IP via the fabric.
func TestApplyOverlayPinsGatewayAddr(t *testing.T) {
	f := readyGatewayFabric()
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{gatewayOverlayNet()},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	if len(f.UnicastGatewayCalls) != 1 {
		t.Fatalf("EnsureUnicastGateway calls = %d, want 1", len(f.UnicastGatewayCalls))
	}
	got := f.UnicastGatewayCalls[0]
	if got.Bridge != "otvb1000" {
		t.Errorf("bridge = %q, want otvb1000", got.Bridge)
	}
	// The tenant IP must carry the overlay subnet's prefix length (/24), never a
	// /32: only the subnet prefix gives the gateway an on-link route to the
	// overlay so it reaches guest VMs instead of leaking out the host default route.
	if got.Addr != netip.MustParsePrefix("10.50.0.7/24") {
		t.Errorf("addr = %v, want 10.50.0.7/24", got.Addr)
	}
	// The bridge must claim the DISTINCT unicast membership MAC, never the shared
	// anycast gateway MAC, so it is a valid unicast FDB target for return traffic.
	if got.MAC != "52:54:00:ab:cd:ef" {
		t.Errorf("mac = %q, want 52:54:00:ab:cd:ef (the unicast membership MAC)", got.MAC)
	}
}

// TestApplyOverlayGatewayAddrIdempotent verifies the gateway addr is re-asserted
// on every reconcile pass with identical arguments, so repeated passes are safe.
func TestApplyOverlayGatewayAddrIdempotent(t *testing.T) {
	f := readyGatewayFabric()
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{gatewayOverlayNet()},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())
	rec.reconcile(context.Background())

	if len(f.UnicastGatewayCalls) != 2 {
		t.Fatalf("EnsureUnicastGateway calls = %d, want 2 (re-asserted each pass)", len(f.UnicastGatewayCalls))
	}
	for i, c := range f.UnicastGatewayCalls {
		if c.Bridge != "otvb1000" || c.Addr != netip.MustParsePrefix("10.50.0.7/24") || c.MAC != "52:54:00:ab:cd:ef" {
			t.Errorf("call %d = %+v, want {otvb1000 10.50.0.7/24 52:54:00:ab:cd:ef}", i, c)
		}
	}
}

// TestApplyOverlayNoGatewayAddrForPlainOverlay verifies a normal overlay network
// (no gateway addr, as a hypervisor node receives it) never pins a gateway addr.
func TestApplyOverlayNoGatewayAddrForPlainOverlay(t *testing.T) {
	f := readyGatewayFabric()
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{overlayNet()},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	if len(f.UnicastGatewayCalls) != 0 {
		t.Errorf("EnsureUnicastGateway called for a network without a gateway addr: %+v", f.UnicastGatewayCalls)
	}
}

// TestApplyGatewayAddrNoSubnetErrors verifies a gateway addr declared without an
// overlay subnet is a hard error, not a silently-installed unroutable /32: the
// tenant IP can only be routed when its subnet prefix length is known.
func TestApplyGatewayAddrNoSubnetErrors(t *testing.T) {
	f := readyGatewayFabric()
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	d := gatewayOverlayNet()
	d.Subnet = nil

	if err := rec.applyGatewayAddr(d); err == nil {
		t.Fatalf("applyGatewayAddr with nil subnet = nil, want error")
	}
	if len(f.UnicastGatewayCalls) != 0 {
		t.Errorf("EnsureUnicastGateway called despite missing subnet: %+v", f.UnicastGatewayCalls)
	}
}
