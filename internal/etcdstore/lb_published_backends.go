// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// ListPublishedLoadBalancerBackends resolves, for every published load balancer
// (PublishedPort != nil), its eligible backend set with each backend's overlay
// address, for push to gateway-role nodes. It is node-independent: every gateway
// forwards to backends wherever they run, so the same set is returned for all
// gateway nodes (unlike ListLoadBalancerHealthTargetsForNode, which filters to a
// node's local backends).
//
// Eligibility mirrors the connect broker's eligibleBackends
// (internal/api/handlers/loadbalancers/connect.go) exactly and fails toward
// INCLUSION: a VM is a backend iff it matches the selector, its runtime is
// observed running, and no fresh Healthy==false record subtracts it. A health
// read error degrades to no-health (keep all); an absent or stale record keeps
// the backend. A backend with no addressable overlay NIC is skipped as not-ready
// (not darkened).
//
// Read-only; O(#published-LBs x #owner-VMs) with per-owner VM-scan memoization -
// the same cluster-wide scan the connect and health-target paths acknowledge; an
// owner/LB index is a tracked fast-follow.
func (s *Store) ListPublishedLoadBalancerBackends(ctx context.Context) ([]store.PublishedLoadBalancer, error) {
	lbs, err := s.ListLoadBalancers(ctx, store.ListLoadBalancersParams{LimitCount: 0})
	if err != nil {
		return nil, err
	}
	// Memoize the owner's VM scan within this one call so N load balancers that
	// share an owner scan the owner's VMs once, not N times.
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

	out := make([]store.PublishedLoadBalancer, 0, len(lbs))
	for _, lb := range lbs {
		if lb.PublishedPort == nil {
			continue // broker-only LB - no published listener
		}
		backends, berr := s.publishedBackends(ctx, lb, ownerVMs)
		if berr != nil {
			return nil, berr
		}
		out = append(out, store.PublishedLoadBalancer{
			LBID:          lb.ID,
			PublishedPort: *lb.PublishedPort,
			Protocol:      lb.Protocol,
			BackendPort:   lb.Port,
			SourceCIDRs:   lb.SourceCIDRs,
			Backends:      backends,
		})
	}
	// Deterministic order (LBID then VMID) so heartbeat payloads are stable
	// across ticks.
	sort.Slice(out, func(i, j int) bool {
		return out[i].LBID.String() < out[j].LBID.String()
	})
	return out, nil
}

// publishedBackends resolves one published LB's eligible backend set, reproducing
// connect.go's eligibleBackends and skipping any eligible VM that has no
// addressable overlay NIC.
func (s *Store) publishedBackends(ctx context.Context, lb store.LoadBalancer, ownerVMs func(uuid.UUID) ([]store.VM, error)) ([]store.PublishedBackend, error) {
	vms, err := ownerVMs(lb.OwnerID)
	if err != nil {
		return nil, err
	}
	health, err := s.ListLBBackendHealth(ctx, lb.ID)
	if err != nil {
		// Health is advisory; a read failure must not dark the LB. Degrade to the
		// phase==running set (fail toward inclusion), mirroring connect.go.
		health = nil
	}
	window := publishedHealthStalenessWindow(lb.HealthCheck, lb.Port)
	now := time.Now()

	backends := make([]store.PublishedBackend, 0, len(vms))
	for _, vm := range vms {
		if !store.SelectorMatches(lb.Selector, vm.Labels) {
			continue
		}
		rt, rerr := s.VMRuntimeByID(ctx, vm.ID)
		if rerr != nil {
			continue // absent or unreadable runtime -> not confirmed up -> exclude
		}
		if rt.Phase != store.VmPhaseRunning {
			continue // not running -> exclude
		}
		// Subtractive health gate, fail toward inclusion: drop only on a record
		// that EXISTS, reports Healthy==false, AND is fresh within the floored
		// window (connect.go's eligibleBackends).
		healthy := true
		if rec, ok := health[vm.ID]; ok {
			if !rec.Healthy && now.Sub(rec.ReportedAt) <= window {
				continue
			}
			healthy = rec.Healthy
		}
		nic, ok, nerr := s.overlayBackendNIC(ctx, vm.ID)
		if nerr != nil {
			return nil, nerr
		}
		if !ok {
			continue // no addressable overlay NIC yet -> not-ready, not darkened
		}
		backends = append(backends, store.PublishedBackend{
			VMID:      vm.ID,
			OverlayIP: *nic.Ipv4Address,
			MAC:       nic.MacAddress,
			Healthy:   healthy,
		})
	}
	sort.Slice(backends, func(i, j int) bool {
		return backends[i].VMID.String() < backends[j].VMID.String()
	})
	return backends, nil
}

// overlayBackendNIC returns the VM's first overlay NIC that carries an IPv4
// address. ok is false when the VM has no addressable overlay NIC (a not-ready
// backend, skipped rather than darkened).
func (s *Store) overlayBackendNIC(ctx context.Context, vmID uuid.UUID) (store.VMNic, bool, error) {
	nics, err := s.ListVMNicsByVM(ctx, vmID)
	if err != nil {
		return store.VMNic{}, false, err
	}
	for _, nic := range nics {
		if nic.Ipv4Address == nil {
			continue
		}
		nw, nerr := s.NetworkByID(ctx, nic.NetworkID)
		if nerr != nil {
			// A NIC referencing a network we cannot read is treated as not-an-
			// overlay-NIC (skip this NIC, keep scanning) rather than bubbling: a
			// single dangling network reference must not dark every published LB.
			// Contrast ListVMNicsByVM above, whose failure means we cannot resolve
			// the VM's NICs at all and so propagates.
			continue
		}
		if nw.Type == store.NetworkTypeOverlay {
			return nic, true, nil
		}
	}
	return store.VMNic{}, false, nil
}

// publishedHealthStalenessWindow duplicates healthStalenessWindow from
// internal/api/handlers/loadbalancers/connect.go (the source of truth): the
// freshness window an observed Healthy==false verdict must fall within to
// subtract a backend. It is HealthCheckStalenessFactor x the effective probe
// interval, FLOORED at HealthCheckHeartbeatFloorSeconds. The floor is
// load-bearing: the observed verdict is CP-stamped on heartbeat receipt and
// cannot advance faster than the agent heartbeat cadence, so a window derived
// from a short probe interval alone would judge a fresh confirmed-unhealthy
// record stale between heartbeats and re-include a confirmed-down backend.
// connect.go's package-local healthStalenessWindow cannot be called from here,
// so the floored expression is reproduced verbatim.
func publishedHealthStalenessWindow(hc store.LoadBalancerHealthCheck, lbPort int32) time.Duration {
	secs := max(hc.EffectiveFor(lbPort).IntervalSeconds, store.HealthCheckHeartbeatFloorSeconds)
	return time.Duration(secs) * time.Second * store.HealthCheckStalenessFactor
}
