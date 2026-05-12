// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package templates

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// forbiddenCreateKeys lists the keys POST /v1/templates rejects with
// 400 forbidden_fields. `visibility` is rejected because Variant C
// fixes new templates to `private`; `owner_id` is server-assigned;
// `id` and the system timestamps / soft-delete column are managed by
// the storage layer. Keep alphabetised.
var forbiddenCreateKeys = []string{
	"created_at",
	"deleted_at",
	"id",
	"owner_id",
	"updated_at",
	"visibility",
}

// Create implements POST /v1/templates. Required permission:
// template:create (admin / operator / developer). The new template is
// owned by the calling user and starts in `visibility = 'private'` -
// see CLAUDE.md. Returns 201 with the projected Template on success.
//
// 409 conflict surfaces a name collision (uq_templates_name); the
// message is intentionally neutral because uniqueness is global, not
// per-owner, and revealing the conflicting owner would be a minor
// info leak.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
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
	if forbidden := validation.ScanForbiddenKeys(body, forbiddenCreateKeys); len(forbidden) > 0 {
		writeForbiddenFields(w, r, forbidden)
		return
	}

	var req createRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeValidation(w, r, "invalid request body")
		return
	}

	params, err := buildCreateParams(caller.ID, &req)
	if err != nil {
		writeValidation(w, r, err.Error())
		return
	}

	row, err := h.store.Queries().CreateTemplate(r.Context(), params)
	if err != nil {
		writeCreateError(w, r, err)
		return
	}

	response.WriteJSON(w, r, http.StatusCreated, toView(row))
}

// buildCreateParams validates the request payload and assembles the
// CreateTemplateParams the SQL layer expects. Defaults are applied
// per the OpenAPI spec; nullable columns (image_size_bytes,
// firmware_id) pass through as `*int64` / `*uuid.UUID`. Validation is
// split into focused helpers (identity / image / firmware / sizing /
// capabilities) so each step stays linear.
func buildCreateParams(ownerID uuid.UUID, req *createRequest) (store.CreateTemplateParams, error) {
	if err := validateCreateIdentity(req); err != nil {
		return store.CreateTemplateParams{}, err
	}
	// image_checksum_sha256 is optional. Empty string или absent →
	// compute mode; the column stays NULL until the worker
	// back-propagates the agent-computed value via
	// UpdateTemplateImageChecksumIfNull. Non-empty values run through
	// the strict 64-char-lowercase-hex validator.
	checksum, err := decodeOptionalChecksum(req.ImageChecksumSHA256)
	if err != nil {
		return store.CreateTemplateParams{}, err
	}
	imgFormat, err := defaultedImageFormat(req)
	if err != nil {
		return store.CreateTemplateParams{}, err
	}
	if err := validateImageSizeOnCreate(req.ImageSizeBytes); err != nil {
		return store.CreateTemplateParams{}, err
	}
	fwType, fwID, err := normaliseFirmwareForCreate(req)
	if err != nil {
		return store.CreateTemplateParams{}, err
	}
	cpu, mem, disk, err := normaliseSizingForCreate(req)
	if err != nil {
		return store.CreateTemplateParams{}, err
	}
	metadata, err := normaliseMetadataForCreate(req.Metadata)
	if err != nil {
		return store.CreateTemplateParams{}, err
	}
	cloudInitUserData, err := decodeNullableString(req.CloudInitUserData)
	if err != nil {
		return store.CreateTemplateParams{}, errors.New("cloud_init_user_data must be null or a string")
	}
	return store.CreateTemplateParams{
		ID:                     uuid.New(),
		OwnerID:                ownerID,
		Name:                   req.Name,
		Description:            valueOrEmpty(req.Description),
		Architecture:           store.CpuArch(req.Architecture),
		OsFamily:               store.OsFamily(req.OSFamily),
		OsVariant:              valueOrEmpty(req.OSVariant),
		ImageUrl:               req.ImageURL,
		ImageChecksumSha256:    checksum,
		ImageFormat:            store.ImageFormat(imgFormat),
		ImageSizeBytes:         req.ImageSizeBytes,
		FirmwareType:           store.FirmwareType(fwType),
		FirmwareID:             fwID,
		DefaultCpuCores:        cpu,
		DefaultMemoryMib:       mem,
		DefaultDiskGib:         disk,
		CloudInitSupported:     valueOr(req.CloudInitSupported, true),
		CloudInitUserData:      cloudInitUserData,
		QemuGuestAgentExpected: valueOr(req.QemuGuestAgentExpected, false),
		Metadata:               metadata,
	}, nil
}

// decodeOptionalChecksum returns the 32-byte digest for а non-empty
// hex string, or nil bytes для an empty input (compute mode). Strict
// validation kicks в only when the operator supplied а value - empty
// string never errors.
func decodeOptionalChecksum(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	return validation.DecodeImageChecksumSHA256(s)
}

// validateCreateIdentity runs the immutable-identity-field validators
// up front: name, architecture, os_family, os_variant, image_url. The
// image checksum is parsed separately because it needs the decoded
// bytes downstream.
func validateCreateIdentity(req *createRequest) error {
	if err := validation.ValidateTemplateName(req.Name); err != nil {
		return err
	}
	if err := validation.ValidateArchitecture(req.Architecture); err != nil {
		return err
	}
	if err := validation.ValidateOSFamily(req.OSFamily); err != nil {
		return err
	}
	if err := validation.ValidateOSVariant(valueOrEmpty(req.OSVariant)); err != nil {
		return err
	}
	return validation.ValidateImageURL(req.ImageURL)
}

// defaultedImageFormat returns the request's image_format or the spec
// default ("qcow2"), validating the chosen value either way.
func defaultedImageFormat(req *createRequest) (string, error) {
	imgFormat := string(store.ImageFormatQcow2)
	if req.ImageFormat != nil {
		imgFormat = *req.ImageFormat
	}
	if err := validation.ValidateImageFormat(imgFormat); err != nil {
		return "", err
	}
	return imgFormat, nil
}

// validateImageSizeOnCreate runs the size validator only when the
// caller supplied a value — image_size_bytes is nullable.
func validateImageSizeOnCreate(size *int64) error {
	if size == nil {
		return nil
	}
	return validation.ValidateImageSizeBytes(*size)
}

// normaliseFirmwareForCreate returns the firmware_type (defaulted to
// "uefi") and parsed firmware_id (nil for an empty / absent string).
func normaliseFirmwareForCreate(req *createRequest) (string, *uuid.UUID, error) {
	fwType := string(store.FirmwareTypeUefi)
	if req.FirmwareType != nil {
		fwType = *req.FirmwareType
	}
	if err := validation.ValidateFirmwareType(fwType); err != nil {
		return "", nil, err
	}
	if req.FirmwareID == nil || *req.FirmwareID == "" {
		return fwType, nil, nil
	}
	parsed, err := uuid.Parse(*req.FirmwareID)
	if err != nil {
		return "", nil, errors.New("firmware_id must be a uuid")
	}
	return fwType, &parsed, nil
}

// normaliseSizingForCreate applies the spec defaults for the three
// sizing fields and validates the resulting values against the column
// CHECK ranges.
func normaliseSizingForCreate(req *createRequest) (int32, int32, int32, error) {
	cpu := valueOr(req.DefaultCPUCores, int32(validation.TemplateDefaultCPU))
	if err := validation.ValidateTemplateCPUCores(int(cpu)); err != nil {
		return 0, 0, 0, err
	}
	mem := valueOr(req.DefaultMemoryMiB, int32(validation.TemplateDefaultMemMiB))
	if err := validation.ValidateTemplateMemoryMiB(int(mem)); err != nil {
		return 0, 0, 0, err
	}
	disk := valueOr(req.DefaultDiskGiB, int32(validation.TemplateDefaultDiskGiB))
	if err := validation.ValidateTemplateDiskGiB(int(disk)); err != nil {
		return 0, 0, 0, err
	}
	return cpu, mem, disk, nil
}

// normaliseMetadataForCreate validates the metadata payload and
// returns the bytes that should land in the JSONB column. An absent
// payload becomes the canonical empty-object literal.
func normaliseMetadataForCreate(raw json.RawMessage) ([]byte, error) {
	if err := validation.ValidateTemplateMetadata(raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return []byte("{}"), nil
	}
	return []byte(raw), nil
}

// valueOr returns *p when p is non-nil, otherwise fallback. Type
// parameter keeps the int32 / bool / string call sites tidy.
func valueOr[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}

// valueOrEmpty returns *p when p is non-nil, otherwise the zero
// string.
func valueOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// writeCreateError maps the post-Insert database error to the standard
// envelope. The interesting branch is the unique-violation on
// uq_templates_name; FK violations on firmware_id are surfaced as
// 400 (the caller is the only party that could have supplied a bogus
// id).
func writeCreateError(w http.ResponseWriter, r *http.Request, err error) {
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
		response.CodeInternal, "persist template", nil)
}
