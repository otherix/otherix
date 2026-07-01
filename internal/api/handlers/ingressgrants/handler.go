// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package ingressgrants hosts the /v1/ingress-grants/* HTTP handlers: the
// operator-facing CRUD for the per-person SSH access grants an external
// user presents at connect time. The whole surface is gated by
// `vm:ingress-grant` (admin/operator any, developer own, viewer none) per
// docs/rbac.md. A grant is a top-level resource (it spans multiple VMs),
// not a VM sub-resource.
//
// Two ownership axes apply for a developer (scope=own):
//   - read / edit / revoke a grant: keyed on the grant's creator. A
//     cross-user grant is invisible (404, never 403, so existence is not
//     leaked).
//   - create / add-vm: each referenced VM must be owned by the caller. A
//     VM the developer can see but does not own yields 403 (capability
//     lack on a visible resource).
//
// The grant token is minted once at creation via auth.GenerateIngressGrantToken
// and surfaced exactly once in the create response; only its hash is
// stored. No handler ever returns the stored hash.
package ingressgrants

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the ingress-grant handlers depend on.
// Depending on the interface rather than the concrete *etcdstore.Store
// narrows the dependency and lets tests substitute a fake.
// *etcdstore.Store satisfies it.
type Store interface {
	CreateIngressGrant(ctx context.Context, arg store.CreateIngressGrantParams) (store.IngressGrant, error)
	IngressGrantByID(ctx context.Context, id uuid.UUID) (store.IngressGrant, error)
	ListIngressGrants(ctx context.Context, arg store.ListIngressGrantsParams) ([]store.IngressGrant, error)
	AddIngressGrantVM(ctx context.Context, grantID uuid.UUID, vm store.IngressGrantVM) (store.IngressGrant, error)
	RemoveIngressGrantVM(ctx context.Context, grantID uuid.UUID, vmName string) (store.IngressGrant, error)
	RevokeIngressGrant(ctx context.Context, grantID uuid.UUID) error
	DeleteIngressGrant(ctx context.Context, grantID uuid.UUID) error
	VMByName(ctx context.Context, name string) (store.VM, error)
}

// Handler bundles the dependencies for the ingress-grant routes.
type Handler struct {
	store Store
	log   *slog.Logger
}

// New constructs a Handler over the given store and logger.
func New(s Store, log *slog.Logger) *Handler {
	return &Handler{store: s, log: log}
}

// grantVMView is one VM entry in a grant's scope.
type grantVMView struct {
	VMName string `json:"vm_name"`
	Ports  []int  `json:"ports"`
	Login  string `json:"login"`
}

// grantView mirrors components/schemas/IngressGrant. The stored token hash is
// intentionally absent; the plaintext token is surfaced only on creation
// through grantCreateResponse.
type grantView struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	RecipientLabel string        `json:"recipient_label"`
	CreatedBy      string        `json:"created_by"`
	VMs            []grantVMView `json:"vms"`
	SourceIP       *string       `json:"source_ip"`
	ExpiresAt      *string       `json:"expires_at"`
	Revoked        bool          `json:"revoked"`
	CreatedAt      string        `json:"created_at"`
	UpdatedAt      string        `json:"updated_at"`
}

// grantCreateResponse extends grantView with the one-time plaintext token
// (mirrors the <X>CreateResponse allOf pattern). The server stores only
// sha256(token); this is the only time the plaintext is returned.
type grantCreateResponse struct {
	grantView
	Token string `json:"token"`
}

// listResponse is the payload for GET /v1/ingress-grants.
type listResponse struct {
	Data []grantView    `json:"data"`
	Meta paginationMeta `json:"meta"`
}

type paginationMeta struct {
	NextCursor *string `json:"next_cursor"`
}

// toView projects a store.IngressGrant onto its public shape, omitting the
// token hash and formatting the nullable expiry as RFC 3339.
func toView(g store.IngressGrant) grantView {
	vms := make([]grantVMView, 0, len(g.VMs))
	for _, vm := range g.VMs {
		vms = append(vms, grantVMView{VMName: vm.VMName, Ports: vm.Ports, Login: vm.Login})
	}
	var expiresAt *string
	if g.ExpiresAt != nil {
		s := g.ExpiresAt.UTC().Format(time.RFC3339Nano)
		expiresAt = &s
	}
	return grantView{
		ID:             g.ID.String(),
		Name:           g.Name,
		RecipientLabel: g.RecipientLabel,
		CreatedBy:      g.CreatedBy.String(),
		VMs:            vms,
		SourceIP:       g.SourceIP,
		ExpiresAt:      expiresAt,
		Revoked:        g.Revoked,
		CreatedAt:      g.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:      g.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}
