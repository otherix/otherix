// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package networks

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// Create implements POST /v1/networks. Required permission:
// network:manage (admin only). Returns 201 with the projected
// Network on success.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}

	if err := validateCreate(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}

	mtu := int32(validation.DefaultMTU)
	if req.MTU != nil {
		mtu = *req.MTU
	}

	cfg := normaliseConfig(req.Config)

	row, err := h.store.Queries().CreateNetwork(r.Context(), store.CreateNetworkParams{
		ID:         uuid.New(),
		Name:       req.Name,
		Type:       store.NetworkType(req.Type),
		BridgeName: req.BridgeName,
		VlanTag:    req.VlanTag,
		Mtu:        mtu,
		Config:     cfg,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "network name already in use", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "persist network", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusCreated, toView(row))
}

// validateCreate enforces the API-edge invariants on the create
// payload. Order is biased toward the most specific error first.
func validateCreate(req *createRequest) error {
	req.Name = strings.TrimSpace(req.Name)
	switch {
	case req.Name == "":
		return errors.New("name is required")
	case utf8.RuneCountInString(req.Name) > validation.NetworkNameMaxLength:
		return errors.New("name is too long")
	}

	if err := validation.ValidateNetworkType(req.Type); err != nil {
		return err
	}

	if err := validation.ValidateBridgeName(req.BridgeName); err != nil {
		return err
	}

	if req.VlanTag != nil {
		if err := validation.ValidateVLANTag(int(*req.VlanTag)); err != nil {
			return err
		}
	}

	if req.MTU != nil {
		if err := validation.ValidateMTU(int(*req.MTU)); err != nil {
			return err
		}
	}

	if err := validateConfigShape(req.Config); err != nil {
		return err
	}

	return nil
}

// validateConfigShape returns nil when the supplied bytes are either
// empty (caller omitted the field) or a JSON object literal. The
// schema column is `jsonb NOT NULL DEFAULT '{}'`; arrays, scalars,
// and the `null` literal are rejected so the column never carries a
// shape downstream code does not expect.
func validateConfigShape(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return errors.New("config must be a JSON object")
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return errors.New("config must be a JSON object")
	}
	if probe == nil {
		return errors.New("config must be a JSON object")
	}
	return nil
}

// normaliseConfig returns the bytes that should land in the
// networks.config column. An absent or empty payload becomes the
// canonical empty-object literal so the column never carries a NULL
// or empty string.
func normaliseConfig(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return []byte(raw)
}
