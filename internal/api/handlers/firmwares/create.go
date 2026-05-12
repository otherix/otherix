// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package firmwares

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// Create implements POST /v1/firmwares. Required permission:
// firmware:manage (admin only). Returns 201 with the projected
// Firmware on success.
//
// 409 conflict is returned for a duplicate (name, architecture) pair
// (uq_firmwares_name_arch with details.field = "name") and for a
// default-firmware collision; the latter carries
// `details.code = "default_already_set"` and
// `details.existing_firmware_id` (or the same `code` without the id
// when the conflict surfaces from a concurrent insert past the
// precheck).
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req createRequest
	if err := json.Unmarshal(body, &req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}
	if err := validateCreate(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}

	arch := store.CpuArch(req.Architecture)
	fwType := store.FirmwareType(req.Type)
	isDefault := req.IsDefault != nil && *req.IsDefault
	secureBoot := req.SecureBoot != nil && *req.SecureBoot

	if isDefault {
		existing, err := h.store.Queries().GetDefaultFirmwareForArchAndType(r.Context(),
			store.GetDefaultFirmwareForArchAndTypeParams{Architecture: arch, Type: fwType})
		switch {
		case err == nil:
			writeDefaultConflict(w, r, existing.ID)
			return
		case errors.Is(err, pgx.ErrNoRows):
			// fall through
		default:
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "load default firmware", nil)
			return
		}
	}

	row, err := h.store.Queries().CreateFirmware(r.Context(), store.CreateFirmwareParams{
		ID:           uuid.New(),
		Name:         req.Name,
		Architecture: arch,
		Type:         fwType,
		Version:      normaliseVersion(req.Version),
		SecureBoot:   secureBoot,
		IsDefault:    isDefault,
	})
	if err != nil {
		writeCreateError(w, r, err)
		return
	}

	response.WriteJSON(w, r, http.StatusCreated, toView(row))
}

// validateCreate enforces the API-edge invariants on the create
// payload. Order is biased toward the most specific error first
// (name → architecture → type → version).
func validateCreate(req *createRequest) error {
	if err := validation.ValidateFirmwareName(req.Name); err != nil {
		return err
	}
	if err := validation.ValidateArchitecture(req.Architecture); err != nil {
		return err
	}
	if err := validation.ValidateFirmwareType(req.Type); err != nil {
		return err
	}
	if req.Version != nil {
		if err := validation.ValidateFirmwareVersion(*req.Version); err != nil {
			return err
		}
	}
	return nil
}

// normaliseVersion converts the typed Version pointer from the create
// payload into the *string the SQL layer expects. An empty string is
// folded to nil — semantically "no version" and consistent with the
// nullable column.
func normaliseVersion(v *string) *string {
	if v == nil || *v == "" {
		return nil
	}
	s := *v
	return &s
}

// writeCreateError maps the post-Insert database error to the standard
// envelope. The interesting branches are the unique-violation paths:
// uq_firmwares_default (rare race past the precheck) and
// uq_firmwares_name_arch.
func writeCreateError(w http.ResponseWriter, r *http.Request, err error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "uq_firmwares_default":
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict,
				"another default firmware exists for this (architecture, type)",
				map[string]any{"code": "default_already_set"})
			return
		case "uq_firmwares_name_arch":
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict,
				"firmware name already in use for this architecture",
				map[string]any{"field": "name"})
			return
		}
	}
	response.WriteError(w, r, http.StatusInternalServerError,
		response.CodeInternal, "persist firmware", nil)
}

// writeDefaultConflict writes the canonical 409 envelope for a
// double-default-firmware attempt. existingID is the id of the
// already-default firmware for the same (architecture, type) pair.
func writeDefaultConflict(w http.ResponseWriter, r *http.Request, existingID uuid.UUID) {
	response.WriteError(w, r, http.StatusConflict,
		response.CodeConflict,
		"another default firmware exists for this (architecture, type); clear it before promoting another",
		map[string]any{
			"code":                 "default_already_set",
			"existing_firmware_id": existingID.String(),
		})
}

// readBody reads the request body in full so the handler can both
// inspect raw JSON keys (for forbidden-field sweeps in PATCH) and
// decode the typed shape. ok=false signals the response has already
// been written.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "could not read body", nil)
		return nil, false
	}
	return body, true
}
