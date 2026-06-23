// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/internal/agent/dhcp4"
	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/netfabric"
)

func strptr(s string) *string { return &s }

// TestNetworksHandleResponseStoresSelfOverlayIP confirms the snapshot the
// reconciler stores carries both the declared networks and the node's own
// overlay IP (self_overlay_ip), which the overlay path needs as the VTEP
// source address.
func TestNetworksHandleResponseStoresSelfOverlayIP(t *testing.T) {
	rec, err := NewNetworks(&netfabric.FakeFabric{}, nil, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	ip := "10.42.0.3/16"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{{ID: "n1", Type: "bridge", Managed: true, BridgeName: "br0", Mtu: 1500}},
		SelfOverlayIP:    &ip,
	})
	d := rec.desired.Load()
	if d == nil {
		t.Fatal("desired not stored")
	}
	if d.selfOverlayIP != ip {
		t.Errorf("selfOverlayIP = %q, want %q", d.selfOverlayIP, ip)
	}
	if len(d.networks) != 1 || d.networks[0].ID != "n1" {
		t.Errorf("networks = %+v, want one entry id n1", d.networks)
	}
}

// TestNewNetworks_RejectsNilFabric confirms boot-time misconfiguration
// surfaces at construction, not at the first reconcile.
func TestNewNetworks_RejectsNilFabric(t *testing.T) {
	_, err := NewNetworks(nil, nil, discardLogger(), 0)
	if !errors.Is(err, ErrNilFabric) {
		t.Errorf("NewNetworks(nil, …) error = %v, want ErrNilFabric", err)
	}
}

// TestNewNetworks_DefaultsTick confirms a zero tick falls back to the
// documented default.
func TestNewNetworks_DefaultsTick(t *testing.T) {
	rec, err := NewNetworks(&netfabric.FakeFabric{}, nil, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	if rec.tick != DefaultTickInterval {
		t.Errorf("tick = %v, want %v", rec.tick, DefaultTickInterval)
	}
}

// TestReconcile_ManagedPlainBridge confirms a managed bridge with no NAT
// drives EnsureBridge(name, mtu) and reports ready, with no gateway or
// masquerade side effects.
func TestReconcile_ManagedPlainBridge(t *testing.T) {
	fab := &netfabric.FakeFabric{}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: "net-1", Name: "flat", Type: "bridge", Managed: true, Egress: "none", BridgeName: "otbr0", Mtu: 1500},
		},
	})
	rec.reconcile(context.Background())

	want := []netfabric.BridgeCall{{Name: "otbr0", MTU: 1500}}
	if diff := cmp.Diff(want, fab.EnsureBridgeCalls); diff != "" {
		t.Errorf("EnsureBridge calls mismatch (-want +got):\n%s", diff)
	}
	if len(fab.GatewayAddrCalls) != 0 || len(fab.MasqueradeCalls) != 0 {
		t.Errorf("unexpected NAT side effects: gw=%v masq=%v", fab.GatewayAddrCalls, fab.MasqueradeCalls)
	}
	reports := rec.NetworkReports()
	if len(reports) != 1 || reports[0].ID != "net-1" || reports[0].ReconciliationStatus != "ready" {
		t.Errorf("NetworkReports = %+v, want one ready entry for net-1", reports)
	}
}

// TestReconcile_ManagedNAT confirms egress=nat materialises bridge +
// gateway address + masquerade and reports ready.
func TestReconcile_ManagedNAT(t *testing.T) {
	fab := &netfabric.FakeFabric{}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{
				ID: "net-nat", Name: "nat0", Type: "bridge", Managed: true,
				Egress: "nat", BridgeName: "otnat0", Mtu: 1500,
				Subnet: strptr("10.20.0.0/24"), Gateway: strptr("10.20.0.1"),
			},
		},
	})
	rec.reconcile(context.Background())

	if diff := cmp.Diff([]netfabric.BridgeCall{{Name: "otnat0", MTU: 1500}}, fab.EnsureBridgeCalls); diff != "" {
		t.Errorf("EnsureBridge mismatch (-want +got):\n%s", diff)
	}
	if len(fab.GatewayAddrCalls) != 1 {
		t.Fatalf("EnsureGatewayAddr calls = %v, want one", fab.GatewayAddrCalls)
	}
	if got := fab.GatewayAddrCalls[0]; got.Bridge != "otnat0" || got.Addr.String() != "10.20.0.1/24" {
		t.Errorf("EnsureGatewayAddr = {%s %s}, want {otnat0 10.20.0.1/24}", got.Bridge, got.Addr)
	}
	if len(fab.MasqueradeCalls) != 1 {
		t.Fatalf("EnsureMasquerade calls = %v, want one", fab.MasqueradeCalls)
	}
	if got := fab.MasqueradeCalls[0]; got.Subnet.String() != "10.20.0.0/24" || got.EgressIface != "" {
		t.Errorf("EnsureMasquerade = {%s %q}, want {10.20.0.0/24 \"\"}", got.Subnet, got.EgressIface)
	}
	if reports := rec.NetworkReports(); len(reports) != 1 || reports[0].ReconciliationStatus != "ready" {
		t.Errorf("NetworkReports = %+v, want one ready entry", reports)
	}
}

// TestReconcile_ManagedNATMissingSubnet confirms egress=nat without
// subnet+gateway fails without touching the fabric NAT primitives.
func TestReconcile_ManagedNATMissingSubnet(t *testing.T) {
	fab := &netfabric.FakeFabric{}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: "net-bad", Name: "nat0", Type: "bridge", Managed: true, Egress: "nat", BridgeName: "otnat0", Mtu: 1500},
		},
	})
	rec.reconcile(context.Background())

	if len(fab.GatewayAddrCalls) != 0 || len(fab.MasqueradeCalls) != 0 {
		t.Errorf("NAT primitives invoked despite missing subnet: gw=%v masq=%v", fab.GatewayAddrCalls, fab.MasqueradeCalls)
	}
	reports := rec.NetworkReports()
	if len(reports) != 1 || reports[0].ReconciliationStatus != "failed" {
		t.Fatalf("NetworkReports = %+v, want one failed entry", reports)
	}
	if reports[0].ReconciliationError == nil {
		t.Errorf("ReconciliationError = nil, want a message")
	}
}

// TestReconcile_UnmanagedExisting confirms an attach-only network whose
// operator bridge exists reports ready and never calls EnsureBridge.
func TestReconcile_UnmanagedExisting(t *testing.T) {
	fab := &netfabric.FakeFabric{BridgeExistsResult: true}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: "net-u", Name: "ext", Type: "bridge", Managed: false, Egress: "none", BridgeName: "br-operator", Mtu: 1500},
		},
	})
	rec.reconcile(context.Background())

	if len(fab.EnsureBridgeCalls) != 0 {
		t.Errorf("EnsureBridge called for unmanaged network: %v", fab.EnsureBridgeCalls)
	}
	if got := fab.BridgeExistsCalls; len(got) != 1 || got[0] != "br-operator" {
		t.Errorf("BridgeExists calls = %v, want [br-operator]", got)
	}
	if reports := rec.NetworkReports(); len(reports) != 1 || reports[0].ReconciliationStatus != "ready" {
		t.Errorf("NetworkReports = %+v, want one ready entry", reports)
	}
}

// TestReconcile_UnmanagedMissing confirms an attach-only network whose
// operator bridge is absent fails and never creates anything.
func TestReconcile_UnmanagedMissing(t *testing.T) {
	fab := &netfabric.FakeFabric{BridgeExistsResult: false}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: "net-u", Name: "ext", Type: "bridge", Managed: false, Egress: "none", BridgeName: "br-missing", Mtu: 1500},
		},
	})
	rec.reconcile(context.Background())

	if len(fab.EnsureBridgeCalls) != 0 {
		t.Errorf("EnsureBridge called for missing unmanaged bridge: %v", fab.EnsureBridgeCalls)
	}
	reports := rec.NetworkReports()
	if len(reports) != 1 || reports[0].ReconciliationStatus != "failed" {
		t.Fatalf("NetworkReports = %+v, want one failed entry", reports)
	}
	if reports[0].ReconciliationError == nil {
		t.Errorf("ReconciliationError = nil, want a message")
	}
}

// TestReconcile_UnsupportedType confirms a non-bridge network fails
// without touching the fabric at all.
func TestReconcile_UnsupportedType(t *testing.T) {
	fab := &netfabric.FakeFabric{}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: "net-ovl", Name: "ovl", Type: "overlay", Managed: true, BridgeName: "x", Mtu: 1500},
		},
	})
	rec.reconcile(context.Background())

	if len(fab.EnsureBridgeCalls) != 0 || len(fab.BridgeExistsCalls) != 0 {
		t.Errorf("fabric touched for unsupported type: ensure=%v exists=%v", fab.EnsureBridgeCalls, fab.BridgeExistsCalls)
	}
	reports := rec.NetworkReports()
	if len(reports) != 1 || reports[0].ReconciliationStatus != "failed" {
		t.Fatalf("NetworkReports = %+v, want one failed entry", reports)
	}
}

// TestReconcile_EnsureBridgeError confirms an EnsureBridge fault is
// captured as a failed report carrying the error text.
func TestReconcile_EnsureBridgeError(t *testing.T) {
	fab := &netfabric.FakeFabric{Errs: map[string]error{"EnsureBridge": errors.New("netlink: permission denied")}}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: "net-1", Name: "flat", Type: "bridge", Managed: true, Egress: "none", BridgeName: "otbr0", Mtu: 1500},
		},
	})
	rec.reconcile(context.Background())

	reports := rec.NetworkReports()
	if len(reports) != 1 {
		t.Fatalf("NetworkReports = %+v, want one entry", reports)
	}
	if reports[0].ReconciliationStatus != "failed" {
		t.Errorf("status = %q, want failed", reports[0].ReconciliationStatus)
	}
	if reports[0].ReconciliationError == nil || *reports[0].ReconciliationError != "netlink: permission denied" {
		t.Errorf("error = %v, want \"netlink: permission denied\"", reports[0].ReconciliationError)
	}
}

// TestReconcile_RemovesManagedBridge confirms a managed NAT network that
// disappears from the declared set is torn down (masquerade, gateway,
// bridge) and dropped from reports.
func TestReconcile_RemovesManagedBridge(t *testing.T) {
	fab := &netfabric.FakeFabric{}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{
				ID: "net-nat", Name: "nat0", Type: "bridge", Managed: true,
				Egress: "nat", BridgeName: "otnat0", Mtu: 1500,
				Subnet: strptr("10.20.0.0/24"), Gateway: strptr("10.20.0.1"),
			},
		},
	})
	rec.reconcile(context.Background())

	// Now declare nothing — the managed NAT network must be torn down.
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{})
	rec.reconcile(context.Background())

	if got := fab.RemoveBridgeCalls; len(got) != 1 || got[0] != "otnat0" {
		t.Errorf("RemoveBridge calls = %v, want [otnat0]", got)
	}
	if got := fab.RemoveMasqCalls; len(got) != 1 || got[0].String() != "10.20.0.0/24" {
		t.Errorf("RemoveMasquerade calls = %v, want [10.20.0.0/24]", got)
	}
	if len(fab.RemoveGatewayCalls) != 1 {
		t.Errorf("RemoveGatewayAddr calls = %v, want one", fab.RemoveGatewayCalls)
	}
	if reports := rec.NetworkReports(); len(reports) != 0 {
		t.Errorf("NetworkReports after removal = %+v, want empty", reports)
	}
}

// TestReconcile_TeardownFailureRetainsAppliedForRetry confirms a managed
// network whose teardown op fails transiently stays in r.applied so the
// next reconcile tick retries the removal (no orphaned bridge leak). Once
// the injected error clears, the next undeclared pass tears it down and
// forgets it. Revert-to-confirm: with the old unconditional
// delete(r.applied, id) the first (failed) pass would forget the id, so
// the "still applied" assertion below fails on the unfixed code.
func TestReconcile_TeardownFailureRetainsAppliedForRetry(t *testing.T) {
	fab := &netfabric.FakeFabric{Errs: map[string]error{"RemoveBridge": errors.New("ebusy")}}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: "net-1", Name: "flat", Type: "bridge", Managed: true, Egress: "none", BridgeName: "otbr0", Mtu: 1500},
		},
	})
	rec.reconcile(context.Background())
	if _, ok := rec.applied["net-1"]; !ok {
		t.Fatalf("applied[net-1] missing after apply; setup failed")
	}

	// Undeclare everything; teardown fails (RemoveBridge -> ebusy).
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{})
	rec.reconcile(context.Background())
	if _, ok := rec.applied["net-1"]; !ok {
		t.Fatalf("applied[net-1] forgotten after FAILED teardown; bridge would orphan with no retry")
	}

	// Clear the error; the next undeclared pass retries and succeeds.
	fab.Errs = nil
	rec.reconcile(context.Background())
	if _, ok := rec.applied["net-1"]; ok {
		t.Errorf("applied[net-1] still present after successful teardown, want forgotten")
	}
}

// TestReconcile_BridgeRenameTearsDownOldBridge confirms re-declaring the
// SAME network id with a different bridge_name tears the OLD managed
// bridge down entirely (RemoveBridge(old)) and ensures the new one.
func TestReconcile_BridgeRenameTearsDownOldBridge(t *testing.T) {
	fab := &netfabric.FakeFabric{}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: "net-1", Name: "flat", Type: "bridge", Managed: true, Egress: "none", BridgeName: "otbr0", Mtu: 1500},
		},
	})
	rec.reconcile(context.Background())

	// Same id, new bridge name.
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: "net-1", Name: "flat", Type: "bridge", Managed: true, Egress: "none", BridgeName: "otbr1", Mtu: 1500},
		},
	})
	rec.reconcile(context.Background())

	if got := fab.RemoveBridgeCalls; len(got) != 1 || got[0] != "otbr0" {
		t.Errorf("RemoveBridge calls = %v, want [otbr0]", got)
	}
	wantEnsure := []netfabric.BridgeCall{{Name: "otbr0", MTU: 1500}, {Name: "otbr1", MTU: 1500}}
	if diff := cmp.Diff(wantEnsure, fab.EnsureBridgeCalls); diff != "" {
		t.Errorf("EnsureBridge calls mismatch (-want +got):\n%s", diff)
	}
	if reports := rec.NetworkReports(); len(reports) != 1 || reports[0].ReconciliationStatus != "ready" {
		t.Errorf("NetworkReports = %+v, want one ready entry", reports)
	}
}

// TestReconcile_NATToNoneTearsDownNAT confirms re-declaring the SAME id
// (same bridge name) from egress=nat to egress=none tears down the old
// masquerade + gateway addr but leaves the bridge in place.
func TestReconcile_NATToNoneTearsDownNAT(t *testing.T) {
	fab := &netfabric.FakeFabric{}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{
				ID: "net-nat", Name: "nat0", Type: "bridge", Managed: true,
				Egress: "nat", BridgeName: "otnat0", Mtu: 1500,
				Subnet: strptr("10.20.0.0/24"), Gateway: strptr("10.20.0.1"),
			},
		},
	})
	rec.reconcile(context.Background())

	// Same id and bridge, egress now none.
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: "net-nat", Name: "nat0", Type: "bridge", Managed: true, Egress: "none", BridgeName: "otnat0", Mtu: 1500},
		},
	})
	rec.reconcile(context.Background())

	if got := fab.RemoveMasqCalls; len(got) != 1 || got[0].String() != "10.20.0.0/24" {
		t.Errorf("RemoveMasquerade calls = %v, want [10.20.0.0/24]", got)
	}
	if len(fab.RemoveGatewayCalls) != 1 {
		t.Errorf("RemoveGatewayAddr calls = %v, want one", fab.RemoveGatewayCalls)
	}
	if len(fab.RemoveBridgeCalls) != 0 {
		t.Errorf("RemoveBridge called on same-bridge nat->none: %v", fab.RemoveBridgeCalls)
	}
	if reports := rec.NetworkReports(); len(reports) != 1 || reports[0].ReconciliationStatus != "ready" {
		t.Errorf("NetworkReports = %+v, want one ready entry", reports)
	}
}

// TestReconcile_NATSubnetChangeReplacesMasquerade confirms re-declaring
// the SAME id (same bridge) with a changed subnet removes the OLD
// masquerade and asserts the NEW one.
func TestReconcile_NATSubnetChangeReplacesMasquerade(t *testing.T) {
	fab := &netfabric.FakeFabric{}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{
				ID: "net-nat", Name: "nat0", Type: "bridge", Managed: true,
				Egress: "nat", BridgeName: "otnat0", Mtu: 1500,
				Subnet: strptr("10.20.0.0/24"), Gateway: strptr("10.20.0.1"),
			},
		},
	})
	rec.reconcile(context.Background())

	// Same id and bridge, subnet A -> B.
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{
				ID: "net-nat", Name: "nat0", Type: "bridge", Managed: true,
				Egress: "nat", BridgeName: "otnat0", Mtu: 1500,
				Subnet: strptr("10.30.0.0/24"), Gateway: strptr("10.30.0.1"),
			},
		},
	})
	rec.reconcile(context.Background())

	if got := fab.RemoveMasqCalls; len(got) != 1 || got[0].String() != "10.20.0.0/24" {
		t.Errorf("RemoveMasquerade calls = %v, want [10.20.0.0/24]", got)
	}
	if len(fab.RemoveBridgeCalls) != 0 {
		t.Errorf("RemoveBridge called on same-bridge subnet change: %v", fab.RemoveBridgeCalls)
	}
	// Two EnsureMasquerade: original A, then B after the change.
	if got := fab.MasqueradeCalls; len(got) != 2 || got[1].Subnet.String() != "10.30.0.0/24" {
		t.Errorf("EnsureMasquerade calls = %v, want second call for 10.30.0.0/24", got)
	}
	if reports := rec.NetworkReports(); len(reports) != 1 || reports[0].ReconciliationStatus != "ready" {
		t.Errorf("NetworkReports = %+v, want one ready entry", reports)
	}
}

// TestReconcile_SteadyStateNoTeardown confirms re-reconciling an
// UNCHANGED managed NAT network triggers ZERO Remove* calls — the delta
// teardown must not fire when nothing changed.
func TestReconcile_SteadyStateNoTeardown(t *testing.T) {
	fab := &netfabric.FakeFabric{}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	net := heartbeat.DeclaredNetwork{
		ID: "net-nat", Name: "nat0", Type: "bridge", Managed: true,
		Egress: "nat", BridgeName: "otnat0", Mtu: 1500,
		Subnet: strptr("10.20.0.0/24"), Gateway: strptr("10.20.0.1"),
	}
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{net},
	})
	rec.reconcile(context.Background())
	// Reconcile the identical declared set again.
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{net},
	})
	rec.reconcile(context.Background())

	if len(fab.RemoveBridgeCalls) != 0 || len(fab.RemoveMasqCalls) != 0 || len(fab.RemoveGatewayCalls) != 0 {
		t.Errorf("steady-state reconcile triggered teardown: removeBridge=%v removeMasq=%v removeGw=%v",
			fab.RemoveBridgeCalls, fab.RemoveMasqCalls, fab.RemoveGatewayCalls)
	}
	if reports := rec.NetworkReports(); len(reports) != 1 || reports[0].ReconciliationStatus != "ready" {
		t.Errorf("NetworkReports = %+v, want one ready entry", reports)
	}
}

// TestReconcile_PartialNATFailureRecordsBridge confirms that when
// EnsureBridge succeeds but a later NAT step fails, the bridge is still
// recorded in r.applied so a subsequent undeclare can reclaim it. Without
// the early record the managed bridge orphans on the host with no GC.
func TestReconcile_PartialNATFailureRecordsBridge(t *testing.T) {
	fab := &netfabric.FakeFabric{Errs: map[string]error{"EnsureMasquerade": errors.New("nft: boom")}}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{
				ID: "net-nat", Name: "nat0", Type: "bridge", Managed: true,
				Egress: "nat", BridgeName: "otnat0", Mtu: 1500,
				Subnet: strptr("10.20.0.0/24"), Gateway: strptr("10.20.0.1"),
			},
		},
	})
	rec.reconcile(context.Background())

	// The report stays failed: NAT has not converged.
	reports := rec.NetworkReports()
	if len(reports) != 1 || reports[0].ReconciliationStatus != "failed" {
		t.Fatalf("NetworkReports = %+v, want one failed entry", reports)
	}
	// The bridge was recorded despite the NAT failure.
	a, ok := rec.applied["net-nat"]
	if !ok {
		t.Fatalf("applied[net-nat] missing; bridge would orphan with no GC")
	}
	if a.BridgeName != "otnat0" || !a.Managed {
		t.Errorf("applied[net-nat] = %+v, want managed bridge otnat0", a)
	}
	// HasNAT must stay false so reconcileDelta does not try to tear down a
	// masquerade that was never installed.
	if a.HasNAT {
		t.Errorf("applied[net-nat].HasNAT = true, want false until NAT converges")
	}

	// Now undeclare everything (still failing). removeUndeclared must reach
	// the recorded bridge and reclaim it.
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{})
	rec.reconcile(context.Background())

	if got := fab.RemoveBridgeCalls; len(got) != 1 || got[0] != "otnat0" {
		t.Errorf("RemoveBridge calls = %v, want [otnat0] (orphan reclaimed)", got)
	}
	if reports := rec.NetworkReports(); len(reports) != 0 {
		t.Errorf("NetworkReports after undeclare = %+v, want empty", reports)
	}
}

// TestReconcile_PartialNATFailureSteadyStateNoTeardown confirms a network
// stuck in partial NAT failure, re-recorded with the same bridge on every
// tick, never triggers a teardown of its own bridge — recording the bridge
// early must not turn the steady-state no-op into a rename/NAT-drop teardown.
func TestReconcile_PartialNATFailureSteadyStateNoTeardown(t *testing.T) {
	fab := &netfabric.FakeFabric{Errs: map[string]error{"EnsureMasquerade": errors.New("nft: boom")}}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	net := heartbeat.DeclaredNetwork{
		ID: "net-nat", Name: "nat0", Type: "bridge", Managed: true,
		Egress: "nat", BridgeName: "otnat0", Mtu: 1500,
		Subnet: strptr("10.20.0.0/24"), Gateway: strptr("10.20.0.1"),
	}
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{net},
	})
	rec.reconcile(context.Background())
	// Same still-failing network reconciled again.
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{net},
	})
	rec.reconcile(context.Background())

	if len(fab.RemoveBridgeCalls) != 0 || len(fab.RemoveMasqCalls) != 0 || len(fab.RemoveGatewayCalls) != 0 {
		t.Errorf("still-failing steady-state triggered teardown: removeBridge=%v removeMasq=%v removeGw=%v",
			fab.RemoveBridgeCalls, fab.RemoveMasqCalls, fab.RemoveGatewayCalls)
	}
	if reports := rec.NetworkReports(); len(reports) != 1 || reports[0].ReconciliationStatus != "failed" {
		t.Errorf("NetworkReports = %+v, want one failed entry", reports)
	}
}

// TestReconcile_DoesNotRemoveUnmanagedBridge confirms an unmanaged
// network that disappears is forgotten without any fabric teardown — an
// operator bridge is never deleted by the agent.
func TestReconcile_DoesNotRemoveUnmanagedBridge(t *testing.T) {
	fab := &netfabric.FakeFabric{BridgeExistsResult: true}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: "net-u", Name: "ext", Type: "bridge", Managed: false, Egress: "none", BridgeName: "br-operator", Mtu: 1500},
		},
	})
	rec.reconcile(context.Background())

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{})
	rec.reconcile(context.Background())

	if len(fab.RemoveBridgeCalls) != 0 {
		t.Errorf("RemoveBridge called for operator bridge: %v", fab.RemoveBridgeCalls)
	}
	if reports := rec.NetworkReports(); len(reports) != 0 {
		t.Errorf("NetworkReports after removal = %+v, want empty", reports)
	}
}

// TestNetworkReports_Sorted confirms NetworkReports returns a snapshot
// sorted by network id.
func TestNetworkReports_Sorted(t *testing.T) {
	fab := &netfabric.FakeFabric{}
	rec, _ := NewNetworks(fab, nil, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: "net-c", Name: "c", Type: "bridge", Managed: true, BridgeName: "brc", Mtu: 1500},
			{ID: "net-a", Name: "a", Type: "bridge", Managed: true, BridgeName: "bra", Mtu: 1500},
			{ID: "net-b", Name: "b", Type: "bridge", Managed: true, BridgeName: "brb", Mtu: 1500},
		},
	})
	rec.reconcile(context.Background())

	reports := rec.NetworkReports()
	got := []string{reports[0].ID, reports[1].ID, reports[2].ID}
	want := []string{"net-a", "net-b", "net-c"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("NetworkReports id order mismatch (-want +got):\n%s", diff)
	}
}

// TestRun_NetworksHandlesTriggerAndTick confirms the run loop reconciles
// on trigger AND on ticker, and exits cleanly on ctx cancellation.
func TestRun_NetworksHandlesTriggerAndTick(t *testing.T) {
	fab := &netfabric.FakeFabric{}
	rec, _ := NewNetworks(fab, nil, discardLogger(), 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = rec.Run(ctx)
	}()

	rec.HandleHeartbeatResponse(ctx, &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: "net-1", Name: "flat", Type: "bridge", Managed: true, BridgeName: "otbr0", Mtu: 1500},
		},
	})

	deadline := time.After(2 * time.Second)
	for len(rec.NetworkReports()) == 0 {
		select {
		case <-deadline:
			t.Fatal("expected trigger-driven reconcile within 2s")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	<-done
}

// managedBridgeDhcpNet returns an egress=none managed bridge with dhcp+dns and a
// subnet - the addressing-island shape (IP + resolver, no default route).
func managedBridgeDhcpNet() heartbeat.DeclaredNetwork {
	subnet := "10.80.0.0/24"
	return heartbeat.DeclaredNetwork{
		ID: "br-svc", Name: "svc", Type: "bridge", Managed: true,
		Egress: "none", BridgeName: "otbsvc", Mtu: 1500,
		Subnet: &subnet, DhcpEnabled: true, DNSEnabled: true,
		Reservations: []heartbeat.DhcpReservation{{MAC: "52:54:00:de:ad:01", IP: "10.80.0.5"}},
	}
}

func TestApplyManaged_DHCPDNSInstallsAnycastServices(t *testing.T) {
	f := &netfabric.FakeFabric{}
	fake := &dhcp4.FakeResponder{}
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{managedBridgeDhcpNet()},
	})
	rec.reconcile(context.Background())

	if len(f.AnycastGatewayCalls) != 1 {
		t.Fatalf("AnycastGatewayCalls = %d, want 1 (anycast gateway plumbed)", len(f.AnycastGatewayCalls))
	}
	if len(f.BridgeRouteCalls) != 1 {
		t.Errorf("BridgeRouteCalls = %d, want 1 (host route to subnet)", len(f.BridgeRouteCalls))
	}
	if len(f.MasqueradeCalls) != 0 {
		t.Errorf("MasqueradeCalls = %d, want 0 (no NAT without egress)", len(f.MasqueradeCalls))
	}
	if len(fake.RegisterCalls) != 1 {
		t.Fatalf("RegisterCalls = %d, want 1 (DHCP responder registered)", len(fake.RegisterCalls))
	}
	cfg := fake.RegisterCalls[0]
	if cfg.Bridge != "otbsvc" {
		t.Errorf("register Bridge = %q, want otbsvc", cfg.Bridge)
	}
	if !cfg.AdvertiseDNS {
		t.Errorf("AdvertiseDNS = false, want true (dns enabled)")
	}
	if cfg.AdvertiseDefaultRoute {
		t.Errorf("AdvertiseDefaultRoute = true, want false (no egress)")
	}
	if !rec.applied["br-svc"].Anycast {
		t.Errorf("applied[br-svc].Anycast = false, want true")
	}
}

func TestApplyManaged_NATDHCPAdvertisesDefaultRoute(t *testing.T) {
	f := &netfabric.FakeFabric{}
	fake := &dhcp4.FakeResponder{}
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	d := managedBridgeDhcpNet()
	d.Egress = "nat"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{d},
	})
	rec.reconcile(context.Background())

	if len(f.MasqueradeCalls) != 1 {
		t.Errorf("MasqueradeCalls = %d, want 1 (NAT masquerade by subnet)", len(f.MasqueradeCalls))
	}
	if f.EnableIPForwardingCalls == 0 {
		t.Errorf("EnableIPForwardingCalls = 0, want >=1")
	}
	if len(fake.RegisterCalls) != 1 || !fake.RegisterCalls[0].AdvertiseDefaultRoute {
		t.Errorf("want one DHCP register advertising the default route, got %+v", fake.RegisterCalls)
	}
}

func TestApplyManaged_PlainBridgeNoServices(t *testing.T) {
	f := &netfabric.FakeFabric{}
	fake := &dhcp4.FakeResponder{}
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{
			{ID: "br-plain", Type: "bridge", Managed: true, Egress: "none", BridgeName: "otbplain", Mtu: 1500},
		},
	})
	rec.reconcile(context.Background())

	if len(f.AnycastGatewayCalls) != 0 {
		t.Errorf("AnycastGatewayCalls = %d, want 0 (no dhcp/dns)", len(f.AnycastGatewayCalls))
	}
	if len(fake.RegisterCalls) != 0 {
		t.Errorf("RegisterCalls = %d, want 0 (no dhcp)", len(fake.RegisterCalls))
	}
	if rec.applied["br-plain"].Anycast {
		t.Errorf("applied[br-plain].Anycast = true, want false")
	}
}

func TestTeardown_ManagedBridgeAnycastDeregistersDHCP(t *testing.T) {
	f := &netfabric.FakeFabric{}
	fake := &dhcp4.FakeResponder{}
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	// First pass: materialise the dhcp/dns bridge.
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{managedBridgeDhcpNet()},
	})
	rec.reconcile(context.Background())
	if len(fake.RegisterCalls) != 1 {
		t.Fatalf("setup: RegisterCalls = %d, want 1", len(fake.RegisterCalls))
	}
	// Second pass: network removed from the declared set.
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{},
	})
	rec.reconcile(context.Background())

	if len(fake.DeregisterCalls) != 1 || fake.DeregisterCalls[0] != "br-svc" {
		t.Errorf("DeregisterCalls = %v, want [br-svc]", fake.DeregisterCalls)
	}
	if len(f.RemoveBridgeCalls) != 1 || f.RemoveBridgeCalls[0] != "otbsvc" {
		t.Errorf("RemoveBridgeCalls = %v, want [otbsvc]", f.RemoveBridgeCalls)
	}
	if _, ok := rec.applied["br-svc"]; ok {
		t.Errorf("applied[br-svc] still present after teardown")
	}
}

// A nat anycast bridge teardown must NOT attempt a real-gateway-addr removal
// (the anycast path installs no in-subnet gateway; a.Gateway is the zero prefix).
func TestTeardown_NATAnycastBridgeSkipsGatewayAddrRemoval(t *testing.T) {
	f := &netfabric.FakeFabric{}
	fake := &dhcp4.FakeResponder{}
	rec, err := NewNetworks(f, fake, discardLogger(), time.Minute)
	if err != nil {
		t.Fatalf("NewNetworks: %v", err)
	}
	d := managedBridgeDhcpNet()
	d.Egress = "nat"
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{d},
	})
	rec.reconcile(context.Background())
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredNetworks: []heartbeat.DeclaredNetwork{},
	})
	rec.reconcile(context.Background())

	if len(f.RemoveGatewayCalls) != 0 {
		t.Errorf("RemoveGatewayCalls = %v, want none (anycast path has no real gateway addr)", f.RemoveGatewayCalls)
	}
	if len(f.RemoveMasqCalls) != 1 {
		t.Errorf("RemoveMasqCalls = %d, want 1 (nat masquerade reclaimed)", len(f.RemoveMasqCalls))
	}
}
