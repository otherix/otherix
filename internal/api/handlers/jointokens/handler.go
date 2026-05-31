// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package jointokens hosts the admin-only HTTP handlers backing the
// /v1/nodes/join-tokens management surface:
//
//   - POST  /v1/nodes/join-tokens                       (Create)
//   - GET   /v1/nodes/join-tokens                       (List)
//   - DELETE /v1/nodes/join-tokens/{id}                 (Delete / revoke)
//   - GET   /v1/nodes/join-tokens/{id}/consumptions     (ListConsumptions)
//
// Every route demands `node:manage` (admin-only per the role matrix).
// The redemption endpoint (POST /v1/nodes/join) lands in Step 2 —
// this package's surface only manages tokens, not consumes them.
//
// Token plaintext is returned exactly once on creation; the server
// stores only sha256(token). The response embeds the active cluster
// CA fingerprint so the operator can hand both to the agent
// simultaneously (TOFU pattern).
package jointokens

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the join-tokens handlers depend on: the
// join-token domain methods plus the active-CA lookup used to embed the
// CA fingerprint in the create response. *etcdstore.Store satisfies it;
// depending on the interface rather than the concrete store narrows the
// handler's storage dependency to the methods it uses and lets tests
// substitute a fake.
type Store interface {
	ActiveCACert(ctx context.Context) (store.CaCert, error)
	JoinTokenByID(ctx context.Context, id uuid.UUID) (store.JoinToken, error)
	CreateJoinToken(ctx context.Context, arg store.CreateJoinTokenParams) (store.JoinToken, error)
	ListJoinTokens(ctx context.Context, arg store.ListJoinTokensParams) ([]store.ListJoinTokensRow, error)
	RevokeJoinToken(ctx context.Context, id uuid.UUID) error
	ListJoinTokenConsumptions(ctx context.Context, arg store.ListJoinTokenConsumptionsParams) ([]store.JoinTokenConsumption, error)
}

// Ensure the production store satisfies the handler's storage contract.

// Handler holds the dependencies of the join-tokens routes.
type Handler struct {
	store Store
	log   *slog.Logger
}

// New constructs the Handler.
func New(s Store, log *slog.Logger) *Handler {
	return &Handler{store: s, log: log}
}

// Validation sentinels used by parseCreateRequest. Each maps to a
// distinct 400 envelope downstream so the operator sees the precise
// failure cause rather than a generic "invalid request body".
var (
	errTTLOutOfRange       = errors.New("ttl_seconds must be in [60, 86400]")
	errMaxUsesNotPositive  = errors.New("max_uses must be >= 1 when set")
	errPreboundMultiUse    = errors.New("pre-bound tokens cannot be reused: set max_uses=1 or omit intended_node_name")
	errIntendedNameTooLong = errors.New("intended_node_name must be at most 253 characters")
)

// errTokenNotFound is the sentinel for a token row missing entirely;
// callers map it to 404.
var errTokenNotFound = errors.New("join token not found")

// loadToken fetches a join token row by id, lifting store.ErrNotFound
// into errTokenNotFound for canonical 404 mapping at the handler
// layer.
func (h *Handler) loadToken(ctx context.Context, id uuid.UUID) (store.JoinToken, error) {
	row, err := h.store.JoinTokenByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.JoinToken{}, errTokenNotFound
		}
		return store.JoinToken{}, err
	}
	return row, nil
}

// activeCAFingerprintHex looks up the active cluster CA row and
// returns the lowercase hex fingerprint. Used by Create to embed the
// "token bundle" CA fingerprint in the response.
func (h *Handler) activeCAFingerprintHex(ctx context.Context) (string, error) {
	row, err := h.store.ActiveCACert(ctx)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(row.FingerprintSha256), nil
}

// toView projects a store.JoinToken plus a consumption count onto
// the public joinTokenView shape. token_hash is never part of the
// projection — even admin callers cannot recover the plaintext from
// the wire surface.
func toView(t store.JoinToken, consumptionCount int64) joinTokenView {
	v := joinTokenView{
		ID:               t.ID.String(),
		ExpiresAt:        t.ExpiresAt.UTC().Format(time.RFC3339Nano),
		ConsumptionCount: consumptionCount,
		CreatedAt:        t.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if t.IntendedNodeName != nil {
		s := *t.IntendedNodeName
		v.IntendedNodeName = &s
	}
	if t.MaxUses != nil {
		n := int64(*t.MaxUses)
		v.MaxUses = &n
	}
	if t.CreatedByUserID != nil {
		s := t.CreatedByUserID.String()
		v.CreatedByUserID = &s
	}
	return v
}

// toViewFromListRow is the variant that accepts the sqlc-generated
// ListJoinTokensRow shape (carries a computed consumption_count).
func toViewFromListRow(row store.ListJoinTokensRow) joinTokenView {
	jt := store.JoinToken{
		ID:               row.ID,
		TokenHash:        row.TokenHash,
		IntendedNodeName: row.IntendedNodeName,
		CreatedByUserID:  row.CreatedByUserID,
		ExpiresAt:        row.ExpiresAt,
		MaxUses:          row.MaxUses,
		CreatedAt:        row.CreatedAt,
	}
	return toView(jt, row.ConsumptionCount)
}

// toConsumptionView projects a store.JoinTokenConsumption onto the
// public shape. source_ip stringification handles the pgtype.Inet
// (here netip.Addr) nullable form.
func toConsumptionView(c store.JoinTokenConsumption) consumptionView {
	v := consumptionView{
		ID:          c.ID.String(),
		JoinTokenID: c.JoinTokenID.String(),
		ConsumedAt:  c.ConsumedAt.UTC().Format(time.RFC3339Nano),
	}
	if c.ConsumedByNodeID != nil {
		s := c.ConsumedByNodeID.String()
		v.ConsumedByNodeID = &s
	}
	if c.SourceIp != nil {
		s := c.SourceIp.String()
		v.SourceIP = &s
	}
	return v
}
