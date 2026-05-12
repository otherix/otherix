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
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Default TTL when the caller omits ttl_seconds; mirrors the
// OpenAPI default (3600 s = 1 hour).
const defaultTTLSeconds = 3600

// intendedNodeNameMaxLength bounds the API-edge length check. 253
// matches the practical DNS-name ceiling и the nodes.name validator
// (defense-in-depth — SQL accepts arbitrary text, but а 4 KB blob
// would be unusable anyway).
const intendedNodeNameMaxLength = 253

// Create implements POST /v1/nodes/join-tokens. Required permission:
// node:manage (admin only — gated by the router-level
// RequirePermission middleware). Mints а fresh token bundle (token +
// CA fingerprint) и returns the plaintext exactly once.
//
// Idempotency-Key intentionally not honored — каждый call mints а
// fresh token (sharing а cached response would compromise the once-
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

	intendedNodeName, ttl, maxUses, err := normaliseCreateRequest(req)
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

	row, err := h.store.Queries().CreateJoinToken(r.Context(), store.CreateJoinTokenParams{
		ID:               uuid.New(),
		TokenHash:        hash,
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
	// the operator-driving fields plus а small projection of the row.
	h.log.InfoContext(r.Context(), "join token created",
		slog.String("token_id", row.ID.String()),
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

// parseCreateRequest decodes the JSON body. Returns а sentinel error
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
func normaliseCreateRequest(req createRequest) (*string, int, *int32, error) {
	ttl := defaultTTLSeconds
	if req.TTLSeconds != nil {
		ttl = *req.TTLSeconds
	}
	if ttl < 60 || ttl > 86400 {
		return nil, 0, nil, errTTLOutOfRange
	}

	var maxUses *int32
	if req.MaxUses != nil {
		if *req.MaxUses < 1 {
			return nil, 0, nil, errMaxUsesNotPositive
		}
		v := *req.MaxUses
		maxUses = &v
	}

	var intendedNodeName *string
	if req.IntendedNodeName != nil {
		trimmed := strings.TrimSpace(*req.IntendedNodeName)
		if trimmed != "" {
			if len(trimmed) > intendedNodeNameMaxLength {
				return nil, 0, nil, errIntendedNameTooLong
			}
			// Pre-bound + multi-use combination — defense-in-depth
			// against the SQL CHECK constraint. Refuses каждое
			// max_uses != 1 (including nil meaning unlimited).
			if maxUses == nil || *maxUses != 1 {
				return nil, 0, nil, errPreboundMultiUse
			}
			intendedNodeName = &trimmed
		}
	}

	return intendedNodeName, ttl, maxUses, nil
}

// writeCreateError maps validation sentinels к 400 envelopes;
// anything else falls к "invalid request body".
func writeCreateError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errTTLOutOfRange):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "ttl_seconds must be в [60, 86400]", nil)
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
	case errors.Is(err, errIntendedNameTooLong):
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "intended_node_name must be at most 253 characters", nil)
	default:
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
	}
}
