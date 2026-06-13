// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package jointokens

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Default TTL when the caller omits ttl_seconds; mirrors the
// OpenAPI default (3600 s = 1 hour).
const defaultTTLSeconds = 3600

// maxClusterTokenUses caps an explicit max_uses on a cluster token. A
// cluster token redeems for the cluster CA private key (the
// highest-value secret), so even a finite-but-large cap is
// operationally equivalent to unlimited. 16 is a generous upper bound
// on the number of CP replicas grown with a single token.
const maxClusterTokenUses = 16

// Create implements POST /v1/nodes/join-tokens. Required permission:
// node:manage (admin only — gated by the router-level
// RequirePermission middleware). Mints a fresh token bundle (token +
// CA fingerprint) and returns the plaintext exactly once.
//
// Idempotency-Key intentionally not honored — each call mints a
// fresh token (sharing a cached response would compromise the once-
// only plaintext invariant). The route is mounted outside the idem
// middleware to enforce this.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	req, err := parseCreateRequest(r)
	if err != nil {
		writeCreateError(w, r, err)
		return
	}

	kind, intendedNodeName, ttl, maxUses, err := normaliseCreateRequest(req)
	if err != nil {
		writeCreateError(w, r, err)
		return
	}

	plaintext, hash, err := auth.GenerateJoinToken()
	if err != nil {
		h.log.ErrorContext(r.Context(), "generate join token", slog.String("error", err.Error()))
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "generate token", nil)
		return
	}

	caFingerprintHex, err := h.activeCAFingerprintHex(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "active CA lookup", slog.String("error", err.Error()))
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "active cluster CA not available", nil)
		return
	}

	expiresAt := time.Now().UTC().Add(time.Duration(ttl) * time.Second)
	callerID := caller.ID

	row, err := h.store.CreateJoinToken(r.Context(), store.CreateJoinTokenParams{
		ID:               uuid.New(),
		TokenHash:        hash,
		Kind:             kind,
		IntendedNodeName: intendedNodeName,
		CreatedByUserID:  &callerID,
		ExpiresAt:        expiresAt,
		MaxUses:          maxUses,
	})
	if err != nil {
		h.log.ErrorContext(r.Context(), "create join token", slog.String("error", err.Error()))
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "persist token", nil)
		return
	}

	// Audit log: INFO-level, never plaintext. Captures
	// the operator-driving fields plus a small projection of the row.
	h.log.InfoContext(r.Context(), "join token created",
		slog.String("token_id", row.ID.String()),
		slog.String("kind", kind),
		slog.String("created_by_user_id", callerID.String()),
		slog.Any("intended_node_name", intendedNodeName),
		slog.Any("max_uses", maxUses),
		slog.Time("expires_at", expiresAt))

	view := toView(row, 0)
	resp := createResponse{
		joinTokenView:       view,
		Token:               plaintext,
		CAFingerprintSHA256: caFingerprintHex,
	}
	response.WriteJSON(w, r, http.StatusCreated, resp)
}

// parseCreateRequest decodes the JSON body. Returns a sentinel error
// suitable for direct status mapping by the caller.
func parseCreateRequest(r *http.Request) (createRequest, error) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, err
	}
	return req, nil
}

// normaliseCreateRequest applies API-edge validation rules: TTL
// bounds, max_uses positivity, pre-bound-single-use, name length.
// Returns the canonicalised values ready for INSERT.
//
// Note: SQL-level CHECK constraints provide defense-in-depth — this
// validation surfaces errors as 400 (not 500 on constraint violation
// at the DB layer).
func normaliseCreateRequest(req createRequest) (kind string, intendedNodeName *string, ttl int, maxUses *int32, err error) {
	kind, err = normaliseKind(req.Kind)
	if err != nil {
		return "", nil, 0, nil, err
	}

	ttl = defaultTTLSeconds
	if req.TTLSeconds != nil {
		ttl = *req.TTLSeconds
	}
	if ttl < 60 || ttl > 86400 {
		return "", nil, 0, nil, errTTLOutOfRange
	}

	maxUses, err = normaliseMaxUses(kind, req.MaxUses)
	if err != nil {
		return "", nil, 0, nil, err
	}

	if req.IntendedNodeName != nil {
		trimmed := strings.TrimSpace(*req.IntendedNodeName)
		if trimmed != "" {
			// A cluster token authorizes a CP replica, not a node, so it
			// carries no node-name binding.
			if kind == store.JoinTokenKindCluster {
				return "", nil, 0, nil, errClusterNodeBound
			}
			// intended_node_name pre-binds the token to a name that becomes
			// the cert CN `node-<name>` at redemption, so constrain it to a
			// lowercase RFC 1123 DNS label (audit LOW). The trimmed,
			// validated value is what gets persisted below.
			if err := validation.ValidateNodeName(trimmed); err != nil {
				return "", nil, 0, nil, errIntendedNameInvalid
			}
			// Pre-bound + multi-use combination — defense-in-depth
			// against the SQL CHECK constraint. Refuses any
			// max_uses != 1.
			if maxUses == nil || *maxUses != 1 {
				return "", nil, 0, nil, errPreboundMultiUse
			}
			intendedNodeName = &trimmed
		}
	}

	return kind, intendedNodeName, ttl, maxUses, nil
}

// normaliseMaxUses canonicalises the requested max_uses. An omitted value defaults to a
// single use for every kind: an unbounded, multi-redemption join credential left live for the
// whole TTL is a footgun, so multi-use must be an explicit opt-in via max_uses=N (audit R2-L3).
// A requested value must be positive; a cluster token additionally caps at maxClusterTokenUses
// (it redeems for the CA private key, so a near-unlimited cap is operationally unlimited).
func normaliseMaxUses(kind string, requested *int32) (*int32, error) {
	if requested == nil {
		one := int32(1)
		return &one, nil
	}
	if *requested < 1 {
		return nil, errMaxUsesNotPositive
	}
	if kind == store.JoinTokenKindCluster && *requested > maxClusterTokenUses {
		return nil, errClusterMaxUsesTooHigh
	}
	v := *requested
	return &v, nil
}

// normaliseKind resolves the optional kind field to a canonical kind, defaulting
// to node (also the reading of an empty string) and rejecting anything else.
func normaliseKind(raw *string) (string, error) {
	if raw == nil {
		return store.JoinTokenKindNode, nil
	}
	switch trimmed := strings.TrimSpace(*raw); trimmed {
	case "", store.JoinTokenKindNode:
		return store.JoinTokenKindNode, nil
	case store.JoinTokenKindCluster:
		return store.JoinTokenKindCluster, nil
	default:
		return "", errInvalidKind
	}
}

// writeCreateError maps validation sentinels to 400 envelopes;
// anything else falls to "invalid request body".
func writeCreateError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errTTLOutOfRange):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "ttl_seconds must be in [60, 86400]", nil)
	case errors.Is(err, errMaxUsesNotPositive):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "max_uses must be >= 1 when set", nil)
	case errors.Is(err, errPreboundMultiUse):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"pre-bound tokens cannot be reused: set max_uses=1 or omit intended_node_name",
			map[string]any{
				"forbidden_combination": []string{"intended_node_name", "max_uses>1"},
			})
	case errors.Is(err, errIntendedNameInvalid):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"intended_node_name must be a lowercase RFC 1123 DNS label ([a-z0-9] and '-', start and end alphanumeric, max 63 characters)",
			map[string]any{"field": "intended_node_name"})
	case errors.Is(err, errInvalidKind):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, `kind must be "node" or "cluster"`, nil)
	case errors.Is(err, errClusterNodeBound):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "cluster tokens cannot carry intended_node_name", nil)
	case errors.Is(err, errClusterMaxUsesTooHigh):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "max_uses for a cluster token must be <= 16",
			map[string]any{"max_uses_ceiling": maxClusterTokenUses})
	default:
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
	}
}
