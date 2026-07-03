// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"net"
	"net/netip"
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

	// HealthCheckHeartbeatFloorSeconds floors the freshness window used to judge
	// an observed health verdict "fresh". The observed verdict (LBBackendHealth)
	// is CP-stamped on heartbeat receipt and shipped only once per heartbeat
	// (default 30s), so ReportedAt cannot advance faster than a heartbeat
	// regardless of how short the probe interval is. Deriving the freshness
	// window from the probe interval alone (e.g. 10s interval x factor 3 = 30s)
	// makes a genuinely confirmed-unhealthy record go "stale" in the gap between
	// heartbeats, so it gets degrade-included and the exclude silently breaks for
	// the default and every shorter interval. Flooring the window at a few
	// heartbeats keeps a short-interval verdict fresh across the heartbeat gap.
	// 30 is the documented default heartbeat interval; a conservative floor - a
	// faster heartbeat just yields a slightly more lenient window, still
	// fail-toward-inclusion.
	HealthCheckHeartbeatFloorSeconds = 30
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
	// PublishedPort, when non-nil, is the public TCP port gateway-role nodes
	// listen on for this LB. Nil means the LB is broker-only (no public
	// listener). Globally unique across published LBs.
	PublishedPort *int32
	// Protocol is the published listener's L4 protocol. Only "tcp" is legal;
	// "" on a pre-feature row normalizes to "tcp" at the view boundary.
	Protocol string
	// SourceCIDRs, when non-empty, restricts the published listener to these
	// client CIDRs (allowlist). Nil/empty means any source that reaches the port.
	SourceCIDRs []string
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
	// PublishedPort, when non-nil, is the public TCP port gateway-role nodes
	// listen on for this LB. Nil means the LB is broker-only (no public
	// listener). Globally unique across published LBs.
	PublishedPort *int32
	// Protocol is the published listener's L4 protocol. Only "tcp" is legal;
	// "" on a pre-feature row normalizes to "tcp" at the view boundary.
	Protocol string
	// SourceCIDRs, when non-empty, restricts the published listener to these
	// client CIDRs (allowlist). Nil/empty means any source that reaches the port.
	SourceCIDRs []string
}

// UpdateLoadBalancerParams is the input to UpdateLoadBalancer. OwnerID is
// immutable, so it is not part of the update surface.
type UpdateLoadBalancerParams struct {
	ID          uuid.UUID
	Name        string
	Port        int32
	Selector    map[string]string
	HealthCheck LoadBalancerHealthCheck
	// PublishedPort, when non-nil, is the public TCP port gateway-role nodes
	// listen on for this LB. Nil means the LB is broker-only (no public
	// listener). Globally unique across published LBs.
	PublishedPort *int32
	// Protocol is the published listener's L4 protocol. Only "tcp" is legal;
	// "" on a pre-feature row normalizes to "tcp" at the view boundary.
	Protocol string
	// SourceCIDRs, when non-empty, restricts the published listener to these
	// client CIDRs (allowlist). Nil/empty means any source that reaches the port.
	SourceCIDRs []string
	// ExpectedRevision, when > 0, gates the update on the primary row's etcd
	// ModRevision (optimistic concurrency). 0 disables the check (back-compat).
	ExpectedRevision int64
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

// PublishedLoadBalancer is a published load balancer plus its resolved backend
// set, computed for push to gateway-role nodes. It is node-independent: every
// gateway forwards to backends wherever they run, so the same set is pushed to
// all gateway nodes (unlike LBHealthTarget, which filters to a node's local
// backends). Computed, not persisted, so its fields carry no struct tags.
type PublishedLoadBalancer struct {
	LBID          uuid.UUID
	PublishedPort int32
	Protocol      string
	BackendPort   int32 // lb.Port (the guest port)
	SourceCIDRs   []string
	Backends      []PublishedBackend
}

// PublishedBackend is one resolved backend of a published load balancer: the
// backend VM plus the overlay address a gateway dials it at. Healthy is the
// observed active-health verdict, informational only - eligibility (fail toward
// inclusion) is already applied, so a backend appears here iff it is eligible.
type PublishedBackend struct {
	VMID      uuid.UUID
	OverlayIP netip.Addr
	MAC       net.HardwareAddr
	Healthy   bool
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
