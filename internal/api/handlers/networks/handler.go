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
	"errors"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the networks handlers depend on.
// Depending on the interface rather than the concrete *etcdstore.Store
// narrows the handler's storage dependency to the methods it uses and lets
// tests substitute a fake. *etcdstore.Store satisfies it.
type Store interface {
	NetworkByID(ctx context.Context, id uuid.UUID) (store.Network, error)
	CreateNetwork(ctx context.Context, arg store.CreateNetworkParams) (store.Network, error)
	UpdateNetwork(ctx context.Context, arg store.UpdateNetworkParams) (store.Network, error)
	ListNetworks(ctx context.Context, arg store.ListNetworksParams) ([]store.Network, error)
	DeleteNetwork(ctx context.Context, id uuid.UUID) error
	ListNetworkNodeStatusByNetwork(ctx context.Context, networkID uuid.UUID) ([]store.NetworkNodeStatus, error)
	NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error)
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
//
// Status is populated only on the GET-by-id path (which fans out to
// ListNetworkNodeStatusByNetwork). The list path leaves it nil so the
// `omitempty` keeps the cheap list response free of per-node status.
type networkView struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	BridgeName string          `json:"bridge_name"`
	Managed    bool            `json:"managed"`
	Egress     string          `json:"egress"`
	VlanTag    *int32          `json:"vlan_tag"`
	MTU        int32           `json:"mtu"`
	Subnet     *string         `json:"subnet"`
	Gateway    *string         `json:"gateway"`
	Config     json.RawMessage `json:"config"`
	CreatedAt  string          `json:"created_at"`
	UpdatedAt  string          `json:"updated_at"`
	Status     *statusView     `json:"status,omitempty"`
}

// statusView is the GET-by-id-only per-node reconciliation rollup
// (components/schemas/Network.status). Each entry is one node that has
// reported a materialisation outcome for the network.
type statusView struct {
	Nodes []networkNodeStatusView `json:"nodes"`
}

// networkNodeStatusView mirrors components/schemas/NetworkNodeStatus.
// NodeID is the stable key; NodeName is the resolved node name surfaced
// alongside it (per the "references use names, not UUIDs" convention),
// left empty when the node no longer exists but a stale status lingers.
type networkNodeStatusView struct {
	NodeID               string  `json:"node_id"`
	NodeName             string  `json:"node_name"`
	ReconciliationStatus string  `json:"reconciliation_status"`
	ReconciliationError  *string `json:"reconciliation_error"`
	LastReconciledAt     *string `json:"last_reconciled_at"`
}

// toView projects a store.Network onto its public networkView. Status
// is left nil; the GET-by-id handler attaches it explicitly.
func toView(n store.Network) networkView {
	return networkView{
		ID:         n.ID.String(),
		Name:       n.Name,
		Type:       string(n.Type),
		BridgeName: n.BridgeName,
		Managed:    n.Managed,
		Egress:     string(n.Egress),
		VlanTag:    n.VlanTag,
		MTU:        n.Mtu,
		Subnet:     prefixString(n.Subnet),
		Gateway:    addrString(n.Gateway),
		Config:     rawJSONOrEmpty(n.Config),
		CreatedAt:  n.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:  n.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// prefixString renders a *netip.Prefix to its canonical CIDR string, or
// nil when the pointer is nil.
func prefixString(p *netip.Prefix) *string {
	if p == nil {
		return nil
	}
	s := p.String()
	return &s
}

// addrString renders a *netip.Addr to its string form, or nil when the
// pointer is nil.
func addrString(a *netip.Addr) *string {
	if a == nil {
		return nil
	}
	s := a.String()
	return &s
}

// nodeStatusViews projects per-node reconciliation rows onto their public
// shape, formatting the nullable timestamp as RFC 3339 and resolving each
// row's node id to its name via NodeByID. Resolution is best-effort: a row
// whose node has been deleted (or never existed) keeps NodeName empty rather
// than erroring - a stale status row must not 500 the network read. NodeID is
// always present as the stable handle. A handful of nodes per network keeps
// this fan-out cheap (the symmetric node view resolves network names the same
// way).
func nodeStatusViews(ctx context.Context, s Store, rows []store.NetworkNodeStatus) ([]networkNodeStatusView, error) {
	views := make([]networkNodeStatusView, 0, len(rows))
	for _, st := range rows {
		v := networkNodeStatusView{
			NodeID:               st.NodeID.String(),
			ReconciliationStatus: st.ReconciliationStatus,
			ReconciliationError:  st.ReconciliationError,
		}
		node, err := s.NodeByID(ctx, st.NodeID)
		switch {
		case err == nil:
			v.NodeName = node.Name
		case errors.Is(err, store.ErrNotFound):
			// Stale status for a deleted node: leave NodeName empty; the
			// client falls back to NodeID.
		default:
			return nil, err
		}
		if st.LastReconciledAt != nil {
			ts := st.LastReconciledAt.UTC().Format(time.RFC3339Nano)
			v.LastReconciledAt = &ts
		}
		views = append(views, v)
	}
	return views, nil
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
