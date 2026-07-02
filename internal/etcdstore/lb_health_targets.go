// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// ListLoadBalancerHealthTargetsForNode returns the (lb, vm, health-check) probe
// targets the given node must actively probe: for every non-deleted load
// balancer, its selector-matched owner VMs whose observed vm_runtime.current_node_id
// is this node. Keying to the current owning node (not the pinned node) is
// migration-aware (ADR 0027) - the source keeps probing until cutover. Read-only;
// O(#LBs x #owner-VMs) - the same cluster-wide scan the connect path acknowledges,
// an owner/LB index is a tracked fast-follow.
func (s *Store) ListLoadBalancerHealthTargetsForNode(ctx context.Context, nodeID uuid.UUID) ([]store.LBHealthTarget, error) {
	lbs, err := s.ListLoadBalancers(ctx, store.ListLoadBalancersParams{LimitCount: 0})
	if err != nil {
		return nil, err
	}
	// Memoize the owner's VM scan within this one call so N load balancers that
	// share an owner scan the owner's VMs once, not N times (a cheap in-request
	// mitigation for the per-node-per-heartbeat cost; the durable fix is an
	// owner/LB index, a near-term fast-follow).
	vmsByOwner := map[uuid.UUID][]store.VM{}
	ownerVMs := func(owner uuid.UUID) ([]store.VM, error) {
		if v, ok := vmsByOwner[owner]; ok {
			return v, nil
		}
		v, verr := s.ListVMsByOwner(ctx, owner)
		if verr != nil {
			return nil, verr
		}
		vmsByOwner[owner] = v
		return v, nil
	}
	var out []store.LBHealthTarget
	for _, lb := range lbs {
		vms, verr := ownerVMs(lb.OwnerID)
		if verr != nil {
			return nil, verr
		}
		hc := lb.HealthCheck.EffectiveFor(lb.Port)
		for _, vm := range vms {
			if !store.SelectorMatches(lb.Selector, vm.Labels) {
				continue
			}
			rt, rerr := s.VMRuntimeByID(ctx, vm.ID)
			if rerr != nil {
				continue // no runtime yet -> not currently on any node -> skip
			}
			if rt.CurrentNodeID == nil || *rt.CurrentNodeID != nodeID {
				continue
			}
			out = append(out, store.LBHealthTarget{
				VMID: vm.ID, VMName: vm.Name, LBID: lb.ID, HealthCheck: hc,
			})
		}
	}
	return out, nil
}
