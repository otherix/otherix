// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

// Health-check default constants and field ranges are the single source of
// truth for the load-balancer active health check (ADR 0027 probe cadence).
const (
	HealthCheckDefaultIntervalSeconds    = 10
	HealthCheckDefaultTimeoutSeconds     = 2
	HealthCheckDefaultHealthyThreshold   = 2
	HealthCheckDefaultUnhealthyThreshold = 3
	HealthCheckStalenessFactor           = 3
)

// LoadBalancerHealthCheck is the per-LB active L4 (TCP-connect) probe config.
// Port==0 is the follow sentinel: the effective probe port is the LB's traffic
// port (so `lb update --port` moves the health port automatically unless the
// operator pinned a non-zero port). A zero IntervalSeconds marks a pre-feature
// row that EffectiveFor normalizes to the defaults.
type LoadBalancerHealthCheck struct {
	Port               int32 // 0 == follow the LB traffic port
	IntervalSeconds    int32
	TimeoutSeconds     int32
	HealthyThreshold   int32
	UnhealthyThreshold int32
}

// DefaultLoadBalancerHealthCheck is the health check stored when a create
// supplies no health_check block: default cadence with the port-follow sentinel.
func DefaultLoadBalancerHealthCheck() LoadBalancerHealthCheck {
	return LoadBalancerHealthCheck{
		Port:               0, // follow the traffic port
		IntervalSeconds:    HealthCheckDefaultIntervalSeconds,
		TimeoutSeconds:     HealthCheckDefaultTimeoutSeconds,
		HealthyThreshold:   HealthCheckDefaultHealthyThreshold,
		UnhealthyThreshold: HealthCheckDefaultUnhealthyThreshold,
	}
}

// EffectiveFor resolves the stored config against the LB's traffic port: a
// zero-value (pre-feature row) normalizes to the defaults, and the port-follow
// sentinel (Port==0) resolves to lbPort. Pure; used by the declared-target
// computation, connect eligibility, and the get view so every consumer sees the
// same effective config.
func (hc LoadBalancerHealthCheck) EffectiveFor(lbPort int32) LoadBalancerHealthCheck {
	if hc.IntervalSeconds == 0 { // pre-feature / zero row
		hc = DefaultLoadBalancerHealthCheck()
	}
	if hc.Port == 0 {
		hc.Port = lbPort
	}
	return hc
}

// LoadBalancer is a user-owned, cluster-wide L4 load balancer addressed by UUID
// with a case-insensitive name uniqueness guard. Selector is the label set a VM
// must carry to be a backend; it serializes natively in the etcd JSON value.
type LoadBalancer struct {
	ID          uuid.UUID
	Name        string
	OwnerID     uuid.UUID
	Port        int32
	Selector    map[string]string
	HealthCheck LoadBalancerHealthCheck
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   *time.Time
}

// CreateLoadBalancerParams is the input to CreateLoadBalancer.
type CreateLoadBalancerParams struct {
	ID          uuid.UUID
	Name        string
	OwnerID     uuid.UUID
	Port        int32
	Selector    map[string]string
	HealthCheck LoadBalancerHealthCheck
}

// UpdateLoadBalancerParams is the input to UpdateLoadBalancer. OwnerID is
// immutable, so it is not part of the update surface.
type UpdateLoadBalancerParams struct {
	ID          uuid.UUID
	Name        string
	Port        int32
	Selector    map[string]string
	HealthCheck LoadBalancerHealthCheck
}

// ListLoadBalancersParams is the input to ListLoadBalancers: an opaque
// (created_at, id) cursor (both nil on the first page) plus a page limit.
type ListLoadBalancersParams struct {
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}

// LBHealthTarget is one active-probe target the CP declares to a node: the load
// balancer, the backend VM, and the effective health-check config the node's
// agent must run against it. Produced per node from the LB selector join,
// filtered to backends whose observed vm_runtime.current_node_id is that node.
type LBHealthTarget struct {
	VMID        uuid.UUID
	VMName      string
	LBID        uuid.UUID
	HealthCheck LoadBalancerHealthCheck
}

// LBBackendHealth is the observed active-health verdict for one (load balancer,
// backend VM) pair, reported by the VM's owning agent through the heartbeat and
// stamped with the CP receive time (the agent's clock is never trusted for
// freshness). Healthy is the agent's debounced verdict; a not-yet-healthy
// (warming) backend and a confirmed-down backend both report Healthy=false.
type LBBackendHealth struct {
	Healthy    bool
	ReportedAt time.Time
}
