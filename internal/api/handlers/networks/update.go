// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package networks

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// Update implements PATCH /v1/networks/{id}. Required permission:
// network:manage (admin only). The handler does a two-step decode:
// it first inspects the raw JSON keys to reject the API-immutable
// `type` field with a 400 forbidden_fields response, then merges
// the typed updateRequest into the existing row.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "id must be a uuid", nil)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "could not read body", nil)
		return
	}

	if err := rejectImmutableKeys(body); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(),
			map[string]any{"forbidden_fields": []string{"type"}})
		return
	}

	var req updateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}

	row, err := h.store.Queries().GetNetworkByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "network not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load network", nil)
		return
	}

	if !applyUpdate(w, r, &row, &req) {
		return
	}

	updated, err := h.store.Queries().UpdateNetwork(r.Context(), store.UpdateNetworkParams{
		ID:         row.ID,
		Name:       row.Name,
		BridgeName: row.BridgeName,
		VlanTag:    row.VlanTag,
		Mtu:        row.Mtu,
		Config:     row.Config,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "network name already in use", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "update network", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, toView(updated))
}

// rejectImmutableKeys scans the top-level JSON object for keys the
// API does not allow on PATCH. Today only `type` qualifies; future
// immutable fields land in this switch.
func rejectImmutableKeys(body []byte) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		// Defer the malformed-body 400 to the strict decode path so
		// the client sees a single, clearer error.
		return nil
	}
	if _, ok := keys["type"]; ok {
		return errors.New("type is immutable")
	}
	return nil
}

// applyUpdate merges req into row in place. Returns false (after
// writing the validation-failed response) when a field is invalid.
func applyUpdate(w http.ResponseWriter, r *http.Request, row *store.Network, req *updateRequest) bool {
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		switch {
		case name == "":
			writeValidation(w, r, "name must not be empty")
			return false
		case utf8.RuneCountInString(name) > validation.NetworkNameMaxLength:
			writeValidation(w, r, "name is too long")
			return false
		}
		row.Name = name
	}
	if req.BridgeName != nil {
		if err := validation.ValidateBridgeName(*req.BridgeName); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		row.BridgeName = *req.BridgeName
	}
	if len(req.VlanTag) > 0 {
		v, err := decodeNullableInt32(req.VlanTag)
		if err != nil {
			writeValidation(w, r, "vlan_tag must be null or an integer")
			return false
		}
		if v != nil {
			if err := validation.ValidateVLANTag(int(*v)); err != nil {
				writeValidation(w, r, err.Error())
				return false
			}
		}
		row.VlanTag = v
	}
	if req.MTU != nil {
		if err := validation.ValidateMTU(int(*req.MTU)); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		row.Mtu = *req.MTU
	}
	if len(req.Config) > 0 {
		if err := validateConfigShape(req.Config); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		row.Config = normaliseConfig(req.Config)
	}
	return true
}

// decodeNullableInt32 unwraps a tri-state RawMessage into an
// optional int32. The literal `null` returns (nil, nil); a JSON
// number returns (&n, nil); anything else surfaces an error so the
// handler can issue a 400.
func decodeNullableInt32(raw json.RawMessage) (*int32, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var n int32
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// writeValidation is a thin shorthand around the validation_failed
// envelope used by applyUpdate.
func writeValidation(w http.ResponseWriter, r *http.Request, msg string) {
	response.WriteError(w, r, http.StatusBadRequest,
		response.CodeValidationFailed, msg, nil)
}
