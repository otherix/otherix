// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package templates

import "encoding/json"

// createRequest is the body of POST /v1/templates. Mirrors
// `TemplateCreate` in api/openapi/control-plane.yaml. `visibility` is
// intentionally absent — Variant C fixes new templates to `private`,
// and the field is rejected by the forbidden-key sweep.
//
// Pointer fields are tri-state: nil means "field omitted" so the
// handler substitutes the spec default; the API surface and the SQL
// columns differ on what counts as a default — see fillCreateDefaults.
type createRequest struct {
	Name                   string          `json:"name"`
	Description            *string         `json:"description,omitempty"`
	Architecture           string          `json:"architecture"`
	OSFamily               string          `json:"os_family"`
	OSVariant              *string         `json:"os_variant,omitempty"`
	ImageURL               string          `json:"image_url"`
	ImageChecksumSHA256    string          `json:"image_checksum_sha256"`
	ImageFormat            *string         `json:"image_format,omitempty"`
	ImageSizeBytes         *int64          `json:"image_size_bytes,omitempty"`
	FirmwareType           *string         `json:"firmware_type,omitempty"`
	FirmwareID             *string         `json:"firmware_id,omitempty"`
	DefaultCPUCores        *int32          `json:"default_cpu_cores,omitempty"`
	DefaultMemoryMiB       *int32          `json:"default_memory_mib,omitempty"`
	DefaultDiskGiB         *int32          `json:"default_disk_gib,omitempty"`
	CloudInitSupported     *bool           `json:"cloud_init_supported,omitempty"`
	CloudInitUserData      json.RawMessage `json:"cloud_init_user_data,omitempty"`
	QemuGuestAgentExpected *bool           `json:"qemu_guest_agent_expected,omitempty"`
	Metadata               json.RawMessage `json:"metadata,omitempty"`
}

// updateRequest is the body of PATCH /v1/templates/{id}. Mirrors
// `TemplateUpdate`. Eleven mutable fields, each tri-state at the JSON
// layer:
//
//   - Pointer-typed fields (Name, Description, OSVariant, FirmwareType,
//     DefaultCPUCores, DefaultMemoryMiB, DefaultDiskGiB,
//     CloudInitSupported, QemuGuestAgentExpected): nil = key absent =
//     leave column as-is.
//   - FirmwareID: tri-state via `json.RawMessage` — length 0 means key
//     absent, the literal `null` clears the column to NULL, a JSON
//     string sets it. Same shape used by networks/firmwares for their
//     nullable fields.
//   - Metadata: `json.RawMessage` whole-replace; absent → leave as-is,
//     present → overwrites the JSONB document (no merge).
//
// Architecture-and-image-source identity fields are absent from the
// struct; the pre-decode sweep rejects them with 400 forbidden_fields.
type updateRequest struct {
	Name                   *string         `json:"name,omitempty"`
	Description            *string         `json:"description,omitempty"`
	OSVariant              *string         `json:"os_variant,omitempty"`
	FirmwareType           *string         `json:"firmware_type,omitempty"`
	FirmwareID             json.RawMessage `json:"firmware_id,omitempty"`
	DefaultCPUCores        *int32          `json:"default_cpu_cores,omitempty"`
	DefaultMemoryMiB       *int32          `json:"default_memory_mib,omitempty"`
	DefaultDiskGiB         *int32          `json:"default_disk_gib,omitempty"`
	CloudInitSupported     *bool           `json:"cloud_init_supported,omitempty"`
	CloudInitUserData      json.RawMessage `json:"cloud_init_user_data,omitempty"`
	QemuGuestAgentExpected *bool           `json:"qemu_guest_agent_expected,omitempty"`
	Metadata               json.RawMessage `json:"metadata,omitempty"`
}

// cloneRequest is the body of POST /v1/templates/{id}/clone. Mirrors
// `TemplateClone`. `description` is tri-state via RawMessage so the
// caller can either inherit the source's description (key absent or
// `null`) or override it (string).
type cloneRequest struct {
	Name        string          `json:"name"`
	Description json.RawMessage `json:"description,omitempty"`
}

// setVisibilityRequest is the body of POST
// /v1/templates/{id}/set-visibility. Mirrors
// `SetTemplateVisibilityRequest`.
type setVisibilityRequest struct {
	Visibility string `json:"visibility"`
}

// listResponse is the payload for GET /v1/templates.
type listResponse struct {
	Data []templateView `json:"data"`
	Meta paginationMeta `json:"meta"`
}

type paginationMeta struct {
	NextCursor *string `json:"next_cursor"`
}
