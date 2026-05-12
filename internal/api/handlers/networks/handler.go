// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package networks hosts the /v1/networks/* HTTP handlers. The full
// CRUD surface is gated by `network:read` (every role) and
// `network:manage` (admin only) per docs/rbac.md. Networks have no
// owner column — the public Network projection is identical for every
// caller, so no dual-shape view is needed.
package networks

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/otherix/otherix/internal/store"
)

// Handler bundles the dependencies for the networks routes.
type Handler struct {
	store *store.Store
	log   *slog.Logger
}

// New constructs a Handler.
func New(s *store.Store, log *slog.Logger) *Handler {
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
