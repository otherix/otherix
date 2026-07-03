// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration

package etcdstore_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// seedBackendNIC writes a vm_nic row on the given network carrying an overlay
// IPv4 address, plus the per-VM and per-network index entries ListVMNicsByVM
// reads. It is the addressable-overlay-NIC fixture the published-backend
// producer resolves a backend's {overlay_ip, mac} from.
func seedBackendNIC(t *testing.T, cli *etcd.Client, vmID, networkID uuid.UUID, ip, mac string) {
	t.Helper()
	ctx := context.Background()
	nicID := uuid.New()
	addr := netip.MustParseAddr(ip)
	nic := store.VMNic{
		ID:          nicID,
		VmID:        vmID,
		NetworkID:   networkID,
		Model:       store.NicModelVirtio,
		MacAddress:  mustMAC(t, mac),
		Ipv4Address: &addr,
		Generation:  1,
	}
	if err := cli.PutJSON(ctx, etcd.Key("vm_nics", nicID.String()), nic); err != nil {
		t.Fatalf("seedBackendNIC: put nic: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "vm_nics", "vm", vmID.String(), nicID.String()), []byte(nicID.String())); err != nil {
		t.Fatalf("seedBackendNIC: put vm index: %v", err)
	}
	if err := cli.Put(ctx, etcd.Key("index", "vm_nics", "network", networkID.String(), nicID.String()), []byte(nicID.String())); err != nil {
		t.Fatalf("seedBackendNIC: put network index: %v", err)
	}
}

// seedBackendVM creates an owner-scoped, labelled VM row, attaches an
// addressable overlay NIC, and binds a running runtime on the node so the
// producer sees a matching+running backend.
func seedBackendVM(t *testing.T, s *etcdstore.Store, cli *etcd.Client, owner, node, network uuid.UUID, labels, ip, mac string) uuid.UUID {
	t.Helper()
	vm := vmRow(uniqueNodeName("pub"))
	vm.OwnerID = owner
	vm.Labels = []byte(labels)
	seedVM(t, cli, vm)
	seedBackendNIC(t, cli, vm.ID, network, ip, mac)
	bindRuntimeToNode(t, s, vm.ID, node)
	return vm.ID
}

func TestListPublishedLoadBalancerBackends(t *testing.T) {
	s, cli := etcdstore.FreshStore(t)
	ctx := context.Background()

	owner := seedLBOwner(t, s)
	node := nodeParams(uniqueNodeName("pub"))
	if _, err := s.CreateNode(ctx, node); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	ov := seedOverlayNetwork(t, s)

	// A matching+running backend with an addressable overlay NIC.
	vmA := seedBackendVM(t, s, cli, owner, node.ID, ov.ID, `{"app":"web"}`, "10.0.0.1", "52:54:00:00:0a:01")
	// A non-matching owner VM (wrong label) - excluded by the selector.
	seedBackendVM(t, s, cli, owner, node.ID, ov.ID, `{"app":"db"}`, "10.0.0.9", "52:54:00:00:0a:09")

	pub := int32(30080)
	lbp := lbParams(uniqueLBName("pub-lb"), owner)
	lbp.Port = 80
	lbp.Selector = map[string]string{"app": "web"}
	lbp.PublishedPort = &pub
	lbp.Protocol = "tcp"
	lbp.SourceCIDRs = []string{"10.0.0.0/8"}
	lb, err := s.CreateLoadBalancer(ctx, lbp)
	if err != nil {
		t.Fatalf("CreateLoadBalancer: %v", err)
	}

	// A broker-only LB (no PublishedPort) must NOT appear in the published set.
	broker := lbParams(uniqueLBName("broker-lb"), owner)
	broker.Selector = map[string]string{"app": "web"}
	if _, err := s.CreateLoadBalancer(ctx, broker); err != nil {
		t.Fatalf("CreateLoadBalancer(broker): %v", err)
	}

	got, err := s.ListPublishedLoadBalancerBackends(ctx)
	if err != nil {
		t.Fatalf("ListPublishedLoadBalancerBackends: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d published LBs, want 1 (broker-only excluded): %+v", len(got), got)
	}
	plb := got[0]
	if plb.LBID != lb.ID {
		t.Errorf("LBID = %v, want %v", plb.LBID, lb.ID)
	}
	if plb.PublishedPort != 30080 {
		t.Errorf("PublishedPort = %d, want 30080", plb.PublishedPort)
	}
	if plb.BackendPort != 80 {
		t.Errorf("BackendPort = %d, want 80 (lb.Port)", plb.BackendPort)
	}
	if plb.Protocol != "tcp" {
		t.Errorf("Protocol = %q, want tcp", plb.Protocol)
	}
	if len(plb.Backends) != 1 {
		t.Fatalf("got %d backends, want 1 (non-matching excluded): %+v", len(plb.Backends), plb.Backends)
	}
	b := plb.Backends[0]
	if b.VMID != vmA {
		t.Errorf("backend VMID = %v, want %v", b.VMID, vmA)
	}
	if b.OverlayIP != netip.MustParseAddr("10.0.0.1") {
		t.Errorf("backend OverlayIP = %v, want 10.0.0.1", b.OverlayIP)
	}
	if b.MAC.String() != "52:54:00:00:0a:01" {
		t.Errorf("backend MAC = %v, want 52:54:00:00:0a:01", b.MAC)
	}
}

// TestListPublishedLoadBalancerBackendsHealthSubtraction is the fail-toward-
// inclusion regression guard: a fresh Healthy==false record subtracts its
// backend, while a backend with NO health record stays included.
func TestListPublishedLoadBalancerBackendsHealthSubtraction(t *testing.T) {
	s, cli := etcdstore.FreshStore(t)
	ctx := context.Background()

	owner := seedLBOwner(t, s)
	node := nodeParams(uniqueNodeName("pub-h"))
	if _, err := s.CreateNode(ctx, node); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	ov := seedOverlayNetwork(t, s)

	// Two matching+running backends. One will carry a fresh unhealthy verdict,
	// the other no health record at all.
	vmDown := seedBackendVM(t, s, cli, owner, node.ID, ov.ID, `{"app":"api"}`, "10.0.1.1", "52:54:00:00:0b:01")
	vmNoRec := seedBackendVM(t, s, cli, owner, node.ID, ov.ID, `{"app":"api"}`, "10.0.1.2", "52:54:00:00:0b:02")

	pub := int32(30081)
	lbp := lbParams(uniqueLBName("pub-h-lb"), owner)
	lbp.Port = 80
	lbp.Selector = map[string]string{"app": "api"}
	lbp.PublishedPort = &pub
	lbp.Protocol = "tcp"
	lb, err := s.CreateLoadBalancer(ctx, lbp)
	if err != nil {
		t.Fatalf("CreateLoadBalancer: %v", err)
	}

	// Fresh, confirmed-not-healthy verdict on vmDown; vmNoRec has none.
	if err := s.UpsertLBBackendHealth(ctx, lb.ID, vmDown, false, time.Now().UTC()); err != nil {
		t.Fatalf("UpsertLBBackendHealth: %v", err)
	}

	got, err := s.ListPublishedLoadBalancerBackends(ctx)
	if err != nil {
		t.Fatalf("ListPublishedLoadBalancerBackends: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d published LBs, want 1: %+v", len(got), got)
	}
	seen := map[uuid.UUID]bool{}
	for _, b := range got[0].Backends {
		seen[b.VMID] = true
	}
	if seen[vmDown] {
		t.Errorf("vmDown %v (fresh unhealthy) must be subtracted, backends=%+v", vmDown, got[0].Backends)
	}
	if !seen[vmNoRec] {
		t.Errorf("vmNoRec %v (no health record) must stay included (fail toward inclusion), backends=%+v", vmNoRec, got[0].Backends)
	}
}

// TestListPublishedLoadBalancerBackendsHealthFloor guards the load-bearing floor
// in publishedHealthStalenessWindow. With the default health check
// (interval=10s), the un-floored window would be 10*StalenessFactor=30s but the
// floored window is max(10,30)*StalenessFactor=90s. A confirmed-unhealthy verdict
// aged 60s is fresh ONLY under the floored window: it must still subtract its
// backend. If the floor were dropped (window collapses to 30s) the 60s verdict
// would be judged stale and the confirmed-down backend re-included - the exact
// regression the floor exists to prevent - and this test would fail.
func TestListPublishedLoadBalancerBackendsHealthFloor(t *testing.T) {
	s, cli := etcdstore.FreshStore(t)
	ctx := context.Background()

	owner := seedLBOwner(t, s)
	node := nodeParams(uniqueNodeName("pub-f"))
	if _, err := s.CreateNode(ctx, node); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	ov := seedOverlayNetwork(t, s)

	vmDown := seedBackendVM(t, s, cli, owner, node.ID, ov.ID, `{"app":"api"}`, "10.0.2.1", "52:54:00:00:0c:01")

	pub := int32(30082)
	lbp := lbParams(uniqueLBName("pub-f-lb"), owner)
	lbp.Port = 80
	lbp.Selector = map[string]string{"app": "api"}
	lbp.PublishedPort = &pub
	lbp.Protocol = "tcp"
	// HealthCheck left zero -> EffectiveFor normalizes to the defaults
	// (interval=10s), so the floored window is 90s and the un-floored 30s.
	lb, err := s.CreateLoadBalancer(ctx, lbp)
	if err != nil {
		t.Fatalf("CreateLoadBalancer: %v", err)
	}

	// Unhealthy verdict aged 60s: inside the floored 90s window, outside the
	// un-floored 30s window. It must subtract only because of the floor.
	reportedAt := time.Now().UTC().Add(-60 * time.Second)
	if err := s.UpsertLBBackendHealth(ctx, lb.ID, vmDown, false, reportedAt); err != nil {
		t.Fatalf("UpsertLBBackendHealth: %v", err)
	}

	got, err := s.ListPublishedLoadBalancerBackends(ctx)
	if err != nil {
		t.Fatalf("ListPublishedLoadBalancerBackends: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d published LBs, want 1: %+v", len(got), got)
	}
	for _, b := range got[0].Backends {
		if b.VMID == vmDown {
			t.Errorf("vmDown %v (unhealthy 60s ago) must be subtracted under the 90s floored window; found included - the floor is not being applied, backends=%+v", vmDown, got[0].Backends)
		}
	}
}
