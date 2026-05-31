// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package networks hosts the /v1/networks/* HTTP handlers. The full
// CRUD surface is gated by `network:read` (every role) and
// `network:manage` (admin only) per docs/rbac.md. Networks have no
// owner column — the public Network projection is identical for every
// caller, so no dual-shape view is needed.
package networks

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the networks handlers depend on.
// Depending on the interface rather than the concrete *store.Store is
// the Phase 2 seam that lets a second backend (Phase 3) be substituted
// under the same handler tests. *store.Store satisfies it.
type Store interface {
	NetworkByID(ctx context.Context, id uuid.UUID) (store.Network, error)
	CreateNetwork(ctx context.Context, arg store.CreateNetworkParams) (store.Network, error)
	UpdateNetwork(ctx context.Context, arg store.UpdateNetworkParams) (store.Network, error)
	ListNetworks(ctx context.Context, arg store.ListNetworksParams) ([]store.Network, error)
	DeleteNetwork(ctx context.Context, id uuid.UUID) error
}

// Ensure the production store satisfies the handler's storage contract.

// Handler bundles the dependencies for the networks routes.
type Handler struct {
	store Store
	log   *slog.Logger
}

// New constructs a Handler. It takes the Store interface so any
// conforming backend can be wired in; production passes *store.Store.
func New(s Store, log *slog.Logger) *Handler {
	return &Handler{store: s, log: log}
}

// networkView mirrors components/schemas/Network in
// api/openapi/control-plane.yaml. The internal soft-delete
// timestamp is intentionally absent.
type networkView struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	BridgeName string          `json:"bridge_name"`
	VlanTag    *int32          `json:"vlan_tag"`
	MTU        int32           `json:"mtu"`
	Config     json.RawMessage `json:"config"`
	CreatedAt  string          `json:"created_at"`
	UpdatedAt  string          `json:"updated_at"`
}

// toView projects a store.Network onto its public networkView.
func toView(n store.Network) networkView {
	return networkView{
		ID:         n.ID.String(),
		Name:       n.Name,
		Type:       string(n.Type),
		BridgeName: n.BridgeName,
		VlanTag:    n.VlanTag,
		MTU:        n.Mtu,
		Config:     rawJSONOrEmpty(n.Config),
		CreatedAt:  n.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:  n.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// rawJSONOrEmpty returns raw if non-empty, otherwise the JSON object
// literal `{}`. networks.config is NOT NULL with a `'{}'` default;
// the helper keeps the wire format honest if the bytes ever come
// back empty (post-restore, unusual rollback paths, etc).
func rawJSONOrEmpty(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return json.RawMessage(raw)
}
