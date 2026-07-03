// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/heartbeat"
)

// gatewayEgressDhcpNet is a gateway overlay (tenant IP + unicast MAC) that ALSO
// declares egress=nat and DHCP. A hypervisor node would react to those by
// bringing up the anycast services plane; a gateway must not.
func gatewayEgressDhcpNet() heartbeat.DeclaredNetwork {
	d := gatewayOverlayNet()
	d.Egress = "nat"
	d.DhcpEnabled = true
	subnet := "10.50.0.0/24"
	d.Subnet = &subnet
	return d
}

// TestGatewayOverlayStripsServicesPlane drives the gateway-mode network
// reconciler over an overlay declared with egress=nat + DHCP and asserts it
// brings up the datapath (bridge + VTEP + the tenant veth) but never touches the
// anycast services plane (anycast gateway, NAT masquerade, IP forwarding, DHCP).
// A gateway hosts no VMs and is never an anycast first-hop router, so the anycast
// services plane is pointless on it; its tenant identity lives on the veth host
// end, not the bridge hardware address.
func TestGatewayOverlayStripsServicesPlane(t *testing.T) {
	f := readyGatewayFabric()
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute, false)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{gatewayEgressDhcpNet()},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	// Datapath is brought up.
	if len(f.EnsureBridgeCalls) == 0 {
		t.Errorf("EnsureBridge not called; gateway must bring up the overlay bridge")
	}
	if len(f.EnsureVXLANCalls) == 0 {
		t.Errorf("EnsureVXLAN not called; gateway must bring up the VTEP")
	}
	if len(f.EnsureVethCalls) != 1 {
		t.Errorf("EnsureVeth calls = %d, want 1 (the gateway tenant veth)", len(f.EnsureVethCalls))
	}

	// Services plane is fully stripped.
	if len(f.AnycastGatewayCalls) != 0 {
		t.Errorf("EnsureAnycastGateway called on a gateway: %+v", f.AnycastGatewayCalls)
	}
	if len(f.MasqueradeIfaceCalls) != 0 {
		t.Errorf("EnsureMasqueradeIface called on a gateway: %+v", f.MasqueradeIfaceCalls)
	}
	if f.EnableIPForwardingCalls != 0 {
		t.Errorf("EnableIPForwarding called %d times on a gateway, want 0", f.EnableIPForwardingCalls)
	}
}

// TestColocatedReconcilerRunsServicesAndVeth proves ONE reconciler with the
// hypervisor capability (hostsVMs=true) drives BOTH planes on the same overlay:
// the ingress veth for its gateway membership AND the overlay services plane
// (NAT masquerade) for its own VMs. A co-located node is a hypervisor that also
// carries a gateway membership on its overlays, so the veth axis and the
// services plane must coexist under a single reconciler.
func TestColocatedReconcilerRunsServicesAndVeth(t *testing.T) {
	f := readyGatewayFabric()
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute, true)
	if err != nil {
		t.Fatalf("NewNetworks() error = %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{gatewayEgressDhcpNet()},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	if len(f.EnsureVethCalls) == 0 {
		t.Errorf("EnsureVethCalls = 0, want >= 1 (co-located node must materialise the ingress veth)")
	}
	if len(f.MasqueradeIfaceCalls) == 0 {
		t.Errorf("MasqueradeIfaceCalls = 0, want >= 1 (co-located node must run overlay services for its own VMs)")
	}
}

// TestNonGatewayOverlayRunsServicesPlane is the contrasting (revert-to-confirm)
// case: the SAME egress=nat + DHCP overlay driven through the ordinary
// (non-gateway) reconciler DOES bring up the anycast services plane. It proves
// the strip above is gateway-mode specific, not an artifact of the input.
func TestNonGatewayOverlayRunsServicesPlane(t *testing.T) {
	f := readyGatewayFabric()
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute, true)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.5/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{gatewayEgressDhcpNet()},
		SelfOverlayIP:    &ip,
	})
	rec.reconcile(context.Background())

	if len(f.AnycastGatewayCalls) != 1 {
		t.Errorf("EnsureAnycastGateway calls = %d, want 1 on a hypervisor node", len(f.AnycastGatewayCalls))
	}
	if len(f.MasqueradeIfaceCalls) != 1 {
		t.Errorf("EnsureMasqueradeIface calls = %d, want 1 on a hypervisor node", len(f.MasqueradeIfaceCalls))
	}
	if f.EnableIPForwardingCalls == 0 {
		t.Errorf("EnableIPForwarding not called on a hypervisor node, want it for egress=nat")
	}
}
