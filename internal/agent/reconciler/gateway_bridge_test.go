// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/agent/dhcp4"
	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
)

// natBridgeNet is a managed type=bridge network with egress=nat but no
// dhcp/dns, exercising the plain-NAT branch of applyManaged (gateway addr +
// masquerade). A hypervisor node reacts to it by pinning the in-subnet gateway
// address and installing SNAT; a gateway must not.
func natBridgeNet() heartbeat.DeclaredNetwork {
	subnet := "10.20.0.0/24"
	gateway := "10.20.0.1"
	return heartbeat.DeclaredNetwork{
		ID: "br-nat", Name: "nat", Type: "bridge", Managed: true,
		Egress: "nat", BridgeName: "otbnat", Mtu: 1500,
		Subnet: &subnet, Gateway: &gateway,
	}
}

// TestGatewayManagedBridgeStripsServicesPlane drives the gateway-mode network
// reconciler over managed type=bridge networks (one dhcp/dns + egress=nat, one
// plain egress=nat) and asserts it runs NONE of the L3/NAT/DHCP services plane,
// nor even a VM-less bridge. A gateway hosts no VMs and is never a first-hop
// router, so a node-local managed bridge has no role on it: the gateway carries
// tenant traffic only over overlays, where bridge VMs are reached through their
// owning agent, never directly through the gateway.
func TestGatewayManagedBridgeStripsServicesPlane(t *testing.T) {
	f := &netfabric.FakeFabric{}
	rec, err := NewNetworks(f, nil, discardLogger(), time.Minute, false)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	svc := managedBridgeDhcpNet()
	svc.Egress = "nat"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{svc, natBridgeNet()},
	})
	rec.reconcile(context.Background())

	if len(f.EnsureBridgeCalls) != 0 {
		t.Errorf("EnsureBridge called on a gateway for a managed bridge: %+v", f.EnsureBridgeCalls)
	}
	if len(f.AnycastGatewayCalls) != 0 {
		t.Errorf("EnsureAnycastGateway called on a gateway: %+v", f.AnycastGatewayCalls)
	}
	if len(f.MasqueradeCalls) != 0 {
		t.Errorf("EnsureMasquerade called on a gateway: %+v", f.MasqueradeCalls)
	}
	if len(f.GatewayAddrCalls) != 0 {
		t.Errorf("EnsureGatewayAddr called on a gateway: %+v", f.GatewayAddrCalls)
	}
	if f.EnableIPForwardingCalls != 0 {
		t.Errorf("EnableIPForwarding called %d times on a gateway, want 0", f.EnableIPForwardingCalls)
	}

	// Both networks still converge: the CP must see the gateway reach ready on a
	// network it correctly does nothing about, not stall on a failed report.
	reps := rec.NetworkReports()
	if len(reps) != 2 {
		t.Fatalf("NetworkReports len = %d, want 2", len(reps))
	}
	for _, rep := range reps {
		if rep.ReconciliationStatus != "ready" {
			t.Errorf("network %s status = %q, want ready", rep.ID, rep.ReconciliationStatus)
		}
	}
}

// TestNonGatewayManagedBridgeRunsServicesPlane is the contrasting
// (revert-to-confirm) case: the SAME managed bridges driven through the
// ordinary (non-gateway) reconciler DO bring up the full services plane. It
// proves the strip above is gateway-mode specific, not an artifact of the input.
func TestNonGatewayManagedBridgeRunsServicesPlane(t *testing.T) {
	f := &netfabric.FakeFabric{}
	fake := &dhcp4.FakeResponder{}
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute, true)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	svc := managedBridgeDhcpNet()
	svc.Egress = "nat"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{svc, natBridgeNet()},
	})
	rec.reconcile(context.Background())

	if len(f.EnsureBridgeCalls) != 2 {
		t.Errorf("EnsureBridgeCalls = %d, want 2 on a hypervisor node", len(f.EnsureBridgeCalls))
	}
	if len(f.AnycastGatewayCalls) != 1 {
		t.Errorf("AnycastGatewayCalls = %d, want 1 on a hypervisor node", len(f.AnycastGatewayCalls))
	}
	if len(f.MasqueradeCalls) != 2 {
		t.Errorf("MasqueradeCalls = %d, want 2 on a hypervisor node", len(f.MasqueradeCalls))
	}
	if len(f.GatewayAddrCalls) != 1 {
		t.Errorf("GatewayAddrCalls = %d, want 1 (plain-nat bridge) on a hypervisor node", len(f.GatewayAddrCalls))
	}
	if f.EnableIPForwardingCalls == 0 {
		t.Errorf("EnableIPForwarding not called on a hypervisor node, want it for egress=nat")
	}
	if len(fake.RegisterCalls) != 1 {
		t.Errorf("RegisterCalls = %d, want 1 (dhcp bridge registered) on a hypervisor node", len(fake.RegisterCalls))
	}
}
