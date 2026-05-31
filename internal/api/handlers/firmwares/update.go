// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package firmwares

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// forbiddenUpdateKeys lists the keys PATCH /v1/firmwares/{id} rejects
// with 400 forbidden_fields. `architecture` and `type` are
// API-immutable identity columns; the timestamps and id are
// system-managed. Keep alphabetised.
var forbiddenUpdateKeys = []string{
	"architecture",
	"created_at",
	"id",
	"type",
	"updated_at",
}

// Update implements PATCH /v1/firmwares/{id}. Required permission:
// firmware:manage (admin only). The handler does a two-step decode:
// it first inspects the raw JSON keys to reject the immutable and
// system-managed fields with 400 forbidden_fields, then merges the
// typed updateRequest into the existing row.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUpdateID(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if forbidden := validation.ScanForbiddenKeys(body, forbiddenUpdateKeys); len(forbidden) > 0 {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"one or more fields cannot be modified",
			map[string]any{"forbidden_fields": forbidden})
		return
	}
	var req updateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}

	row, err := h.store.FirmwareByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "firmware not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load firmware", nil)
		return
	}

	if !applyUpdate(w, r, &row, &req) {
		return
	}

	if !h.checkDefaultPromotion(w, r, &req, row) {
		return
	}

	updated, err := h.store.UpdateFirmware(r.Context(), store.UpdateFirmwareParams{
		ID:         row.ID,
		Name:       row.Name,
		Version:    row.Version,
		SecureBoot: row.SecureBoot,
		IsDefault:  row.IsDefault,
	})
	if err != nil {
		writeUpdateError(w, r, err)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, toView(updated))
}

// parseUpdateID extracts and validates the path id. ok=false signals
// the response has already been written.
func parseUpdateID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "id must be a uuid", nil)
		return uuid.Nil, false
	}
	return id, true
}

// applyUpdate merges req into row in place. Returns false (after
// writing the validation_failed response) when any field is invalid.
func applyUpdate(w http.ResponseWriter, r *http.Request, row *store.Firmware, req *updateRequest) bool {
	if req.Name != nil {
		if err := validation.ValidateFirmwareName(*req.Name); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		row.Name = *req.Name
	}
	if len(req.Version) > 0 {
		v, err := decodeNullableString(req.Version)
		if err != nil {
			writeValidation(w, r, "version must be null or a string")
			return false
		}
		if v != nil {
			if err := validation.ValidateFirmwareVersion(*v); err != nil {
				writeValidation(w, r, err.Error())
				return false
			}
		}
		row.Version = normaliseVersion(v)
	}
	if req.SecureBoot != nil {
		row.SecureBoot = *req.SecureBoot
	}
	if req.IsDefault != nil {
		row.IsDefault = *req.IsDefault
	}
	return true
}

// checkDefaultPromotion enforces the at-most-one-default invariant
// for PATCH bodies that flip is_default to true on a row that was
// previously not the default. Returns false (after writing the
// conflict envelope) when another default firmware already exists for
// the same (architecture, type) pair. Clearing or no-op transitions
// short-circuit and return true.
//
// `row.IsDefault` here reflects the post-merge state (applyUpdate
// already wrote into it); `row` was loaded as the pre-merge state
// before applyUpdate ran, so we capture the original IsDefault by
// checking req.IsDefault directly: a real promotion is "request says
// true AND original row was false", and the loaded row was already
// shadowed. Instead we recompute by looking at req: if req.IsDefault
// is non-nil and true, and the row PRIOR to apply was not default,
// the precheck must run. To avoid passing two rows around we instead
// always run the precheck when req asks for is_default=true; the
// query returns this row itself as the "existing" default if the row
// is already the default for the pair, in which case we skip the
// conflict (existing.ID == row.ID).
func (h *Handler) checkDefaultPromotion(w http.ResponseWriter, r *http.Request, req *updateRequest, row store.Firmware) bool {
	if req.IsDefault == nil || !*req.IsDefault {
		return true
	}
	existing, err := h.store.DefaultFirmwareForArchType(r.Context(), row.Architecture, row.Type)
	switch {
	case err == nil:
		if existing.ID != row.ID {
			writeDefaultConflict(w, r, existing.ID)
			return false
		}
		return true
	case errors.Is(err, store.ErrNotFound):
		return true
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load default firmware", nil)
		return false
	}
}

// writeUpdateError maps the post-UPDATE database error to the standard
// envelope. Mirrors writeCreateError.
func writeUpdateError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrFirmwareDefaultExists):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict,
			"another default firmware exists for this (architecture, type)",
			map[string]any{"code": "default_already_set"})
	case errors.Is(err, store.ErrFirmwareNameExists):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict,
			"firmware name already in use for this architecture",
			map[string]any{"field": "name"})
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "update firmware", nil)
	}
}

// writeValidation is a thin shorthand around the validation_failed
// envelope used by applyUpdate.
func writeValidation(w http.ResponseWriter, r *http.Request, msg string) {
	response.WriteError(w, r, http.StatusBadRequest,
		response.CodeValidationFailed, msg, nil)
}

// decodeNullableString unwraps a tri-state RawMessage into an
// optional string. The literal `null` returns (nil, nil); a JSON
// string returns (&s, nil); anything else surfaces an error so the
// handler can issue a 400.
func decodeNullableString(raw json.RawMessage) (*string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
