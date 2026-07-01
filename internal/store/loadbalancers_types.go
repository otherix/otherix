// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

// LoadBalancer is a user-owned, cluster-wide L4 load balancer addressed by UUID
// with a case-insensitive name uniqueness guard. Selector is the label set a VM
// must carry to be a backend; it serializes natively in the etcd JSON value.
type LoadBalancer struct {
	ID        uuid.UUID
	Name      string
	OwnerID   uuid.UUID
	Port      int32
	Selector  map[string]string
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

// CreateLoadBalancerParams is the input to CreateLoadBalancer.
type CreateLoadBalancerParams struct {
	ID       uuid.UUID
	Name     string
	OwnerID  uuid.UUID
	Port     int32
	Selector map[string]string
}

// UpdateLoadBalancerParams is the input to UpdateLoadBalancer. OwnerID is
// immutable, so it is not part of the update surface.
type UpdateLoadBalancerParams struct {
	ID       uuid.UUID
	Name     string
	Port     int32
	Selector map[string]string
}

// ListLoadBalancersParams is the input to ListLoadBalancers: an opaque
// (created_at, id) cursor (both nil on the first page) plus a page limit.
type ListLoadBalancersParams struct {
	CursorCreatedAt *time.Time
	CursorID        *uuid.UUID
	LimitCount      int32
}
