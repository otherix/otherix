// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import (
	"time"

	"github.com/otherix/otherix/internal/store"
)

// createRequest is the body of POST /v1/loadbalancers. owner_id is intentionally
// absent: the owner is stamped from the authenticated caller, never the body.
type createRequest struct {
	Name        string              `json:"name"`
	Port        int32               `json:"port"`
	Selector    map[string]string   `json:"selector"`
	HealthCheck *healthCheckRequest `json:"health_check,omitempty"`
}

// updateRequest is the body of PATCH /v1/loadbalancers/{id}. Every field is a
// pointer so an omitted key leaves the stored value as-is; a present key sets
// it. owner_id is not patchable.
type updateRequest struct {
	Name        *string             `json:"name,omitempty"`
	Port        *int32              `json:"port,omitempty"`
	Selector    *map[string]string  `json:"selector,omitempty"`
	HealthCheck *healthCheckRequest `json:"health_check,omitempty"`
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

// loadBalancerView is the public projection of a store.LoadBalancer. The
// internal soft-delete timestamp is intentionally absent.
type loadBalancerView struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	OwnerID     string            `json:"owner_id"`
	Port        int32             `json:"port"`
	Selector    map[string]string `json:"selector"`
	HealthCheck healthCheckView   `json:"health_check"`
	Backends    []backendView     `json:"backends"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
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
		// Backends is enumerated only by the single-resource get (h.buildBackends
		// overwrites this); the list projection leaves the empty array so the
		// wire shape always carries a backends array, never null.
		Backends:  []backendView{},
		CreatedAt: lb.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: lb.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}
