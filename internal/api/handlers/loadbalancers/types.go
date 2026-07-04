// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import (
	"time"

	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// createRequest is the body of POST /v1/loadbalancers. owner_id is intentionally
// absent: the owner is stamped from the authenticated caller, never the body.
type createRequest struct {
	Name        string              `json:"name"`
	Port        int32               `json:"port"`
	Selector    map[string]string   `json:"selector"`
	HealthCheck *healthCheckRequest `json:"health_check,omitempty"`
	// PublishedPort, when present, exposes the load balancer on that public TCP
	// port via gateway-role nodes. Setting it requires loadbalancer:publish.
	PublishedPort *int32 `json:"published_port,omitempty"`
	// Protocol is the published listener's L4 protocol; only "tcp" is accepted.
	// Empty defaults to "tcp" when a published port is set.
	Protocol string `json:"protocol,omitempty"`
	// SourceCIDRs, when non-empty, restricts the published listener to these
	// client source ranges.
	SourceCIDRs []string `json:"source_cidrs,omitempty"`
}

// updateRequest is the body of PATCH /v1/loadbalancers/{id}. Every field is a
// pointer so an omitted key leaves the stored value as-is; a present key sets
// it. owner_id is not patchable.
type updateRequest struct {
	Name        *string             `json:"name,omitempty"`
	Port        *int32              `json:"port,omitempty"`
	Selector    *map[string]string  `json:"selector,omitempty"`
	HealthCheck *healthCheckRequest `json:"health_check,omitempty"`
	// PublishedPort is tri-state: an omitted key leaves the stored port as-is; a
	// present non-zero value publishes on that port; the value 0 is the unpublish
	// sentinel that clears the port, protocol, and source CIDRs together.
	PublishedPort *int32 `json:"published_port,omitempty"`
	// Protocol, when present, sets the published listener's L4 protocol.
	Protocol *string `json:"protocol,omitempty"`
	// SourceCIDRs, when present, replaces the source-range allowlist (an empty
	// array clears it, opening the listener to all sources).
	SourceCIDRs *[]string `json:"source_cidrs,omitempty"`
}

// healthCheckRequest is the optional health_check block on create/update. Every
// field is a pointer so an omitted key takes the default (create) or leaves the
// stored value (update).
type healthCheckRequest struct {
	Port               *int32 `json:"port,omitempty"`
	IntervalSeconds    *int32 `json:"interval_seconds,omitempty"`
	TimeoutSeconds     *int32 `json:"timeout_seconds,omitempty"`
	HealthyThreshold   *int32 `json:"healthy_threshold,omitempty"`
	UnhealthyThreshold *int32 `json:"unhealthy_threshold,omitempty"`
}

// healthCheckView is the effective (sentinel-resolved) health-check config
// surfaced on a loadBalancerView.
type healthCheckView struct {
	Port               int32 `json:"port"`
	IntervalSeconds    int32 `json:"interval_seconds"`
	TimeoutSeconds     int32 `json:"timeout_seconds"`
	HealthyThreshold   int32 `json:"healthy_threshold"`
	UnhealthyThreshold int32 `json:"unhealthy_threshold"`
}

// backendView is one entry in a loadBalancerView's backends array: a VM the
// selector currently matches, with its latest observed active-health verdict.
// Healthy and ReportedAt are nil when no health record exists yet (a warming
// backend), which renders as JSON null and is distinguishable from a confirmed
// healthy: false.
type backendView struct {
	VMID       string  `json:"vm_id"`
	VMName     string  `json:"vm_name"`
	Healthy    *bool   `json:"healthy"`
	ReportedAt *string `json:"reported_at"`
}

// listenerStatusView is one entry in a published load balancer's listeners
// array: the observed bind status of the public listener on one gateway node.
// Error carries the failure string only when Bound is false (omitted otherwise).
type listenerStatusView struct {
	NodeID     string `json:"node_id"`
	Port       int32  `json:"port"`
	Bound      bool   `json:"bound"`
	Error      string `json:"error,omitempty"`
	ReportedAt string `json:"reported_at"`
}

// healthSummaryView is the aggregate active-health rollup of a load balancer's
// currently-matched backends: an overall Status plus the healthy/total target
// counts. It lets an operator read load-balancer health at a glance without
// enumerating every backend.
type healthSummaryView struct {
	Status         string `json:"status"` // healthy | degraded | unhealthy | no_backends
	TargetsTotal   int    `json:"targets_total"`
	TargetsHealthy int    `json:"targets_healthy"`
}

// loadBalancerView is the public projection of a store.LoadBalancer. The
// internal soft-delete timestamp is intentionally absent.
type loadBalancerView struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	OwnerID     string            `json:"owner_id"`
	Port        int32             `json:"port"`
	Selector    map[string]string `json:"selector"`
	HealthCheck healthCheckView   `json:"health_check"`
	// PublishedPort is nil (renders JSON null) when the load balancer is not
	// published on a public port.
	PublishedPort *int32        `json:"published_port"`
	Protocol      string        `json:"protocol"`
	SourceCIDRs   []string      `json:"source_cidrs"`
	Backends      []backendView `json:"backends"`
	// Listeners is the observed per-gateway bind status of the published
	// listener, present only when the load balancer is published (get only). A
	// status older than the heartbeat-floored freshness window is omitted, so a
	// dead gateway's last-reported row does not read as a live listener.
	Listeners []listenerStatusView `json:"listeners,omitempty"`
	// Health is the aggregate active-health rollup, populated only by get and
	// list (which have live-health context). toView leaves it nil, so
	// create/update responses omit it.
	Health    *healthSummaryView `json:"health,omitempty"`
	CreatedAt string             `json:"created_at"`
	UpdatedAt string             `json:"updated_at"`
}

// listResponse is the payload for GET /v1/loadbalancers.
type listResponse struct {
	Data []loadBalancerView `json:"data"`
	Meta paginationMeta     `json:"meta"`
}

type paginationMeta struct {
	NextCursor *string `json:"next_cursor"`
}

// toView projects a store.LoadBalancer onto its public loadBalancerView.
func toView(lb store.LoadBalancer) loadBalancerView {
	hc := lb.HealthCheck.EffectiveFor(lb.Port)
	// An empty stored protocol predates the published-port feature or was never
	// published; surface the canonical default so the field is never blank.
	protocol := lb.Protocol
	if protocol == "" {
		protocol = validation.DefaultLBProtocol
	}
	return loadBalancerView{
		ID:       lb.ID.String(),
		Name:     lb.Name,
		OwnerID:  lb.OwnerID.String(),
		Port:     lb.Port,
		Selector: lb.Selector,
		HealthCheck: healthCheckView{
			Port:               hc.Port,
			IntervalSeconds:    hc.IntervalSeconds,
			TimeoutSeconds:     hc.TimeoutSeconds,
			HealthyThreshold:   hc.HealthyThreshold,
			UnhealthyThreshold: hc.UnhealthyThreshold,
		},
		PublishedPort: lb.PublishedPort,
		Protocol:      protocol,
		SourceCIDRs:   lb.SourceCIDRs,
		// Backends is enumerated only by the single-resource get (h.buildBackends
		// overwrites this); the list projection leaves the empty array so the
		// wire shape always carries a backends array, never null.
		Backends:  []backendView{},
		CreatedAt: lb.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: lb.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}
