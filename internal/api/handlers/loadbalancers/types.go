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
	Name     string            `json:"name"`
	Port     int32             `json:"port"`
	Selector map[string]string `json:"selector"`
}

// updateRequest is the body of PATCH /v1/loadbalancers/{id}. Every field is a
// pointer so an omitted key leaves the stored value as-is; a present key sets
// it. owner_id is not patchable.
type updateRequest struct {
	Name     *string            `json:"name,omitempty"`
	Port     *int32             `json:"port,omitempty"`
	Selector *map[string]string `json:"selector,omitempty"`
}

// loadBalancerView is the public projection of a store.LoadBalancer. The
// internal soft-delete timestamp is intentionally absent.
type loadBalancerView struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	OwnerID   string            `json:"owner_id"`
	Port      int32             `json:"port"`
	Selector  map[string]string `json:"selector"`
	CreatedAt string            `json:"created_at"`
	UpdatedAt string            `json:"updated_at"`
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
	return loadBalancerView{
		ID:        lb.ID.String(),
		Name:      lb.Name,
		OwnerID:   lb.OwnerID.String(),
		Port:      lb.Port,
		Selector:  lb.Selector,
		CreatedAt: lb.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: lb.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}
