// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package templates

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// forbiddenUpdateKeys lists the keys PATCH /v1/templates/{id} rejects
// with 400 forbidden_fields. Three groups:
//
//   - Identity (immutable): architecture, os_family, owner_id.
//   - Image-source bundle (immutable; clone to evolve): image_url,
//     image_checksum_sha256, image_format, image_size_bytes.
//   - Visibility — only setVisibility may flip it.
//   - System-managed: id, created_at, updated_at, deleted_at.
//
// Keep alphabetised.
var forbiddenUpdateKeys = []string{
	"architecture",
	"created_at",
	"deleted_at",
	"id",
	"image_checksum_sha256",
	"image_format",
	"image_size_bytes",
	"image_url",
	"os_family",
	"owner_id",
	"updated_at",
	"visibility",
}

// Update implements PATCH /v1/templates/{id}. Required permission:
// `template:update` (admin/operator: any; developer: own). The
// handler does a two-step decode: it first inspects the raw JSON keys
// to reject the immutable / system-managed / visibility fields with
// 400 forbidden_fields, then merges the typed updateRequest into the
// loaded row. {id} is a template name; UUID literals are rejected
// with 400 validation_failed at the resolver.
//
// Cross-user developer attempts surface as 404 (no leak), not 403.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if forbidden := validation.ScanForbiddenKeys(body, forbiddenUpdateKeys); len(forbidden) > 0 {
		writeForbiddenFields(w, r, forbidden)
		return
	}

	var req updateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeValidation(w, r, "invalid request body")
		return
	}

	row, err := resolver.Template(r.Context(), h.store.Queries(), chi.URLParam(r, "id"))
	if err != nil {
		writeLoadError(w, r, err)
		return
	}

	if err := auth.CheckOwnership(caller, &row.OwnerID, auth.PermTemplateUpdate); err != nil {
		if errors.Is(err, auth.ErrPermissionDenied) {
			writeNotFound(w, r)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "ownership check", nil)
		return
	}

	if !applyUpdate(w, r, &row, &req) {
		return
	}

	updated, err := h.store.Queries().UpdateTemplate(r.Context(), store.UpdateTemplateParams{
		ID:                     row.ID,
		Name:                   row.Name,
		Description:            row.Description,
		OsVariant:              row.OsVariant,
		FirmwareType:           row.FirmwareType,
		FirmwareID:             row.FirmwareID,
		DefaultCpuCores:        row.DefaultCpuCores,
		DefaultMemoryMib:       row.DefaultMemoryMib,
		DefaultDiskGib:         row.DefaultDiskGib,
		CloudInitSupported:     row.CloudInitSupported,
		CloudInitUserData:      row.CloudInitUserData,
		QemuGuestAgentExpected: row.QemuGuestAgentExpected,
		Metadata:               row.Metadata,
	})
	if err != nil {
		writeUpdateError(w, r, err)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, toView(updated))
}

// applyUpdate merges req into row in place. Returns false (after
// writing the validation_failed response) when any field is invalid.
// Split into focused helpers (identity / firmware / sizing /
// capabilities / metadata) so each step stays linear and gocyclo
// stays under the project ceiling.
func applyUpdate(w http.ResponseWriter, r *http.Request, row *store.Template, req *updateRequest) bool {
	if !applyIdentityPatches(w, r, row, req) {
		return false
	}
	if !applyFirmwarePatches(w, r, row, req) {
		return false
	}
	if !applySizingPatches(w, r, row, req) {
		return false
	}
	if !applyCapabilityPatches(w, r, row, req) {
		return false
	}
	return applyMetadataPatch(w, r, row, req)
}

// applyIdentityPatches handles name / description / os_variant.
func applyIdentityPatches(w http.ResponseWriter, r *http.Request, row *store.Template, req *updateRequest) bool {
	if req.Name != nil {
		if err := validation.ValidateTemplateName(*req.Name); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		row.Name = *req.Name
	}
	if req.Description != nil {
		row.Description = *req.Description
	}
	if req.OSVariant != nil {
		if err := validation.ValidateOSVariant(*req.OSVariant); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		row.OsVariant = *req.OSVariant
	}
	return true
}

// applyFirmwarePatches handles firmware_type and the tri-state
// firmware_id (omit / null / uuid).
func applyFirmwarePatches(w http.ResponseWriter, r *http.Request, row *store.Template, req *updateRequest) bool {
	if req.FirmwareType != nil {
		if err := validation.ValidateFirmwareType(*req.FirmwareType); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		row.FirmwareType = store.FirmwareType(*req.FirmwareType)
	}
	if len(req.FirmwareID) == 0 {
		return true
	}
	s, err := decodeNullableString(req.FirmwareID)
	if err != nil {
		writeValidation(w, r, "firmware_id must be null or a string")
		return false
	}
	if s == nil {
		row.FirmwareID = nil
		return true
	}
	parsed, err := uuid.Parse(*s)
	if err != nil {
		writeValidation(w, r, "firmware_id must be a uuid")
		return false
	}
	row.FirmwareID = &parsed
	return true
}

// applySizingPatches handles default_cpu_cores / _memory_mib / _disk_gib.
func applySizingPatches(w http.ResponseWriter, r *http.Request, row *store.Template, req *updateRequest) bool {
	if req.DefaultCPUCores != nil {
		if err := validation.ValidateTemplateCPUCores(int(*req.DefaultCPUCores)); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		row.DefaultCpuCores = *req.DefaultCPUCores
	}
	if req.DefaultMemoryMiB != nil {
		if err := validation.ValidateTemplateMemoryMiB(int(*req.DefaultMemoryMiB)); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		row.DefaultMemoryMib = *req.DefaultMemoryMiB
	}
	if req.DefaultDiskGiB != nil {
		if err := validation.ValidateTemplateDiskGiB(int(*req.DefaultDiskGiB)); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		row.DefaultDiskGib = *req.DefaultDiskGiB
	}
	return true
}

// applyCapabilityPatches handles cloud_init_supported,
// cloud_init_user_data (L3) and qemu_guest_agent_expected. The two
// boolean fields are tri-state pointers (omit / set); user-data is
// tri-state via json.RawMessage (omit / null / string) к support
// explicit clearing.
func applyCapabilityPatches(w http.ResponseWriter, r *http.Request, row *store.Template, req *updateRequest) bool {
	if req.CloudInitSupported != nil {
		row.CloudInitSupported = *req.CloudInitSupported
	}
	if len(req.CloudInitUserData) > 0 {
		s, err := decodeNullableString(req.CloudInitUserData)
		if err != nil {
			writeValidation(w, r, "cloud_init_user_data must be null or a string")
			return false
		}
		row.CloudInitUserData = s
	}
	if req.QemuGuestAgentExpected != nil {
		row.QemuGuestAgentExpected = *req.QemuGuestAgentExpected
	}
	return true
}

// applyMetadataPatch handles metadata as a whole-JSONB replace. An
// absent key leaves the column as-is; a present payload overwrites it.
func applyMetadataPatch(w http.ResponseWriter, r *http.Request, row *store.Template, req *updateRequest) bool {
	if len(req.Metadata) == 0 {
		return true
	}
	if err := validation.ValidateTemplateMetadata(req.Metadata); err != nil {
		writeValidation(w, r, err.Error())
		return false
	}
	row.Metadata = []byte(req.Metadata)
	return true
}

// writeUpdateError maps the post-UPDATE database error to the standard
// envelope. The interesting branches are uq_templates_name (rename
// collision) and a stray firmware_id FK violation.
func writeUpdateError(w http.ResponseWriter, r *http.Request, err error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			if pgErr.ConstraintName == "uq_templates_name" {
				writeNeutralNameConflict(w, r)
				return
			}
		case "23503":
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed,
				"firmware_id does not reference an existing firmware", nil)
			return
		}
	}
	response.WriteError(w, r, http.StatusInternalServerError,
		response.CodeInternal, "update template", nil)
}
