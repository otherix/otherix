// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package loadbalancers hosts the /v1/loadbalancers/* HTTP handlers. Load
// balancers are user-owned, cluster-wide L4 forwarders addressed by NAME (like
// VMs), not by UUID. The CRUD surface is gated by the loadbalancer:read /
// :create / :update / :delete permissions per docs/rbac.md; update and delete
// additionally run auth.CheckOwnership, and a cross-owner attempt surfaces as
// 404 (existence is never leaked).
package loadbalancers

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/vms"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the load-balancer handlers depend on. Depending
// on the interface rather than the concrete *etcdstore.Store narrows the
// dependency to the methods actually used and lets tests substitute a fake.
// *etcdstore.Store satisfies it.
type Store interface {
	CreateLoadBalancer(ctx context.Context, arg store.CreateLoadBalancerParams) (store.LoadBalancer, error)
	LoadBalancerByName(ctx context.Context, name string) (store.LoadBalancer, error)
	// LoadBalancerByNameWithRevision resolves the row and its etcd ModRevision so
	// an update can be gated on it (optimistic concurrency); the Update handler
	// threads the revision into UpdateLoadBalancerParams and retries on a conflict.
	LoadBalancerByNameWithRevision(ctx context.Context, name string) (store.LoadBalancer, int64, error)
	UpdateLoadBalancer(ctx context.Context, arg store.UpdateLoadBalancerParams) (store.LoadBalancer, error)
	ListLoadBalancers(ctx context.Context, arg store.ListLoadBalancersParams) ([]store.LoadBalancer, error)
	DeleteLoadBalancer(ctx context.Context, id uuid.UUID) error

	// ListVMsByOwner and VMRuntimeByID back the connect route's backend
	// selection: the owner's VMs (desired state) filtered by the observed
	// runtime phase.
	ListVMsByOwner(ctx context.Context, ownerID uuid.UUID) ([]store.VM, error)
	VMRuntimeByID(ctx context.Context, vmID uuid.UUID) (store.VMRuntime, error)

	// ListLBBackendHealth returns the observed active-health verdicts for a load
	// balancer's backends, keyed by backend VM id. Connect eligibility uses it to
	// subtract confirmed-not-healthy backends; an absent or stale record leaves a
	// running backend included (health is advisory, per ADR 0027).
	ListLBBackendHealth(ctx context.Context, lbID uuid.UUID) (map[uuid.UUID]store.LBBackendHealth, error)
}

// IngressBroker resolves connect coordinates for a (vm, port). It is held so
// the connect route (added in a later task) can broker a session; the CRUD
// handlers do not call it. *vms.Handler satisfies it structurally, which keeps
// the dependency one-way (this package imports vms for the IngressResult type;
// vms does not import this package, so there is no cycle).
type IngressBroker interface {
	ResolveIngress(ctx context.Context, vm store.VM, port int) (vms.IngressResult, error)
}

// Handler bundles the dependencies for the load-balancer routes.
type Handler struct {
	store  Store
	broker IngressBroker
	log    *slog.Logger
}

// New constructs a Handler over the given Store and IngressBroker.
func New(s Store, broker IngressBroker, log *slog.Logger) *Handler {
	return &Handler{store: s, broker: broker, log: log}
}
