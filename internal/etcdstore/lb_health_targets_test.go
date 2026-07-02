// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration

package etcdstore_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

func TestListLoadBalancerHealthTargetsForNode(t *testing.T) {
	s, cli := etcdstore.FreshStore(t)
	ctx := context.Background()

	owner := seedLBOwner(t, s)
	node := nodeParams(uniqueNodeName("hc"))
	if _, err := s.CreateNode(ctx, node); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	// Two matching, owner-O VMs whose observed runtime is on the node.
	vmA := vmRow(uniqueNodeName("hc-a"))
	vmA.OwnerID = owner
	vmA.Labels = []byte(`{"app":"web"}`)
	seedVM(t, cli, vmA)
	vmB := vmRow(uniqueNodeName("hc-b"))
	vmB.OwnerID = owner
	vmB.Labels = []byte(`{"app":"web"}`)
	seedVM(t, cli, vmB)

	// A non-matching owner-O VM (wrong label) - must be excluded by the selector.
	vmC := vmRow(uniqueNodeName("hc-c"))
	vmC.OwnerID = owner
	vmC.Labels = []byte(`{"app":"db"}`)
	seedVM(t, cli, vmC)

	bindRuntimeToNode(t, s, vmA.ID, node.ID)
	bindRuntimeToNode(t, s, vmB.ID, node.ID)
	bindRuntimeToNode(t, s, vmC.ID, node.ID)

	// An LB owned by O selecting app=web on traffic port 80 with a custom health
	// check pinned to port 8080, 5s interval.
	lbp := lbParams(uniqueLBName("hc-lb"), owner)
	lbp.Port = 80
	lbp.Selector = map[string]string{"app": "web"}
	lbp.HealthCheck = store.LoadBalancerHealthCheck{
		Port: 8080, IntervalSeconds: 5, TimeoutSeconds: 2, HealthyThreshold: 2, UnhealthyThreshold: 3,
	}
	lb, err := s.CreateLoadBalancer(ctx, lbp)
	if err != nil {
		t.Fatalf("CreateLoadBalancer: %v", err)
	}

	got, err := s.ListLoadBalancerHealthTargetsForNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListLoadBalancerHealthTargetsForNode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d targets, want 2: %+v", len(got), got)
	}
	seen := map[uuid.UUID]store.LBHealthTarget{}
	for _, tgt := range got {
		seen[tgt.VMID] = tgt
	}
	for _, want := range []uuid.UUID{vmA.ID, vmB.ID} {
		tgt, ok := seen[want]
		if !ok {
			t.Fatalf("target for VM %v missing from %+v", want, got)
		}
		if tgt.LBID != lb.ID {
			t.Errorf("target %v LBID = %v, want %v", want, tgt.LBID, lb.ID)
		}
		if tgt.HealthCheck.Port != 8080 {
			t.Errorf("target %v HealthCheck.Port = %d, want 8080", want, tgt.HealthCheck.Port)
		}
		if tgt.HealthCheck.IntervalSeconds != 5 {
			t.Errorf("target %v HealthCheck.IntervalSeconds = %d, want 5", want, tgt.HealthCheck.IntervalSeconds)
		}
	}
	if _, leaked := seen[vmC.ID]; leaked {
		t.Errorf("non-matching VM %v leaked into targets", vmC.ID)
	}
}

// TestListLoadBalancerHealthTargetsForNodeFiltersOtherNode proves a
// selector-matching backend whose observed runtime is on a DIFFERENT node is not
// returned for this node.
func TestListLoadBalancerHealthTargetsForNodeFiltersOtherNode(t *testing.T) {
	s, cli := etcdstore.FreshStore(t)
	ctx := context.Background()

	owner := seedLBOwner(t, s)
	node := nodeParams(uniqueNodeName("hc-this"))
	if _, err := s.CreateNode(ctx, node); err != nil {
		t.Fatalf("CreateNode(this): %v", err)
	}
	other := nodeParams(uniqueNodeName("hc-other"))
	if _, err := s.CreateNode(ctx, other); err != nil {
		t.Fatalf("CreateNode(other): %v", err)
	}

	vm := vmRow(uniqueNodeName("hc-elsewhere"))
	vm.OwnerID = owner
	vm.Labels = []byte(`{"app":"web"}`)
	seedVM(t, cli, vm)
	bindRuntimeToNode(t, s, vm.ID, other.ID)

	lbp := lbParams(uniqueLBName("hc-lb2"), owner)
	lbp.Port = 80
	lbp.Selector = map[string]string{"app": "web"}
	if _, err := s.CreateLoadBalancer(ctx, lbp); err != nil {
		t.Fatalf("CreateLoadBalancer: %v", err)
	}

	got, err := s.ListLoadBalancerHealthTargetsForNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("ListLoadBalancerHealthTargetsForNode: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d targets for node without the backend, want 0: %+v", len(got), got)
	}
}

// bindRuntimeToNode upserts a running vm_runtime row for vmID whose
// current_node_id is nodeID, via the real heartbeat projection write path.
func bindRuntimeToNode(t *testing.T, s *etcdstore.Store, vmID, nodeID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	n := nodeID
	if err := s.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		return hp.UpsertVMRuntime(ctx, store.UpsertVMRuntimeParams{
			VmID: vmID, CurrentNodeID: &n, Phase: store.VmPhaseRunning, ObservedGeneration: 1,
		})
	}); err != nil {
		t.Fatalf("bind runtime for %v -> %v: %v", vmID, nodeID, err)
	}
}
