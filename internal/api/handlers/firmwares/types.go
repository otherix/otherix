// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package firmwares

import "encoding/json"

// createRequest is the body of POST /v1/firmwares. Mirrors
// FirmwareCreate in api/openapi/control-plane.yaml.
//
// Version is *string so a client may either omit the key or send
// JSON null to leave the column NULL on insert; either form maps to
// nil. SecureBoot and IsDefault are *bool so an absent key
// materialises to false without ambiguity.
type createRequest struct {
	Name         string  `json:"name"`
	Architecture string  `json:"architecture"`
	Type         string  `json:"type"`
	Version      *string `json:"version,omitempty"`
	SecureBoot   *bool   `json:"secure_boot,omitempty"`
	IsDefault    *bool   `json:"is_default,omitempty"`
}

// updateRequest is the body of PATCH /v1/firmwares/{id}. Mirrors
// FirmwareUpdate. Field-level semantics:
//
//   - Name / SecureBoot / IsDefault: nil pointer = leave as-is, non-nil
//     = set.
//   - Version: tri-state via plain (non-pointer) json.RawMessage —
//     length 0 means the key was absent; the JSON literal `null` clears
//     the column to NULL; a JSON string sets it. The networks handler
//     uses the same shape for its tri-state vlan_tag.
//
// `architecture` and `type` are intentionally ABSENT from the struct;
// the pre-decode key sweep rejects them with 400 forbidden_fields.
// `id`, `created_at`, `updated_at` are likewise rejected.
type updateRequest struct {
	Name       *string         `json:"name,omitempty"`
	Version    json.RawMessage `json:"version,omitempty"`
	SecureBoot *bool           `json:"secure_boot,omitempty"`
	IsDefault  *bool           `json:"is_default,omitempty"`
}

// listResponse is the payload for GET /v1/firmwares.
type listResponse struct {
	Data []firmwareView `json:"data"`
	Meta paginationMeta `json:"meta"`
}

type paginationMeta struct {
	NextCursor *string `json:"next_cursor"`
}
