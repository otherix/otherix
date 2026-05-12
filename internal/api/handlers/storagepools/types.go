// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import "encoding/json"

// createRequest is the body of POST /v1/storage-pools. Mirrors
// StoragePoolCreate in api/openapi/control-plane.yaml.
//
// `node` is a node name; UUID literals are rejected with 400
// validation_failed at the resolver layer
// (internal/api/handlers/internal/resolver).
//
// There is no per-row `is_default` flag - cluster default-pool is
// held в cluster_settings.default_pool_name. POST bodies carrying
// `is_default` are rejected by the forbidden-fields key sweep.
//
// Config is RawMessage so the handler can persist it verbatim into the
// jsonb column. The agent-reported fields (`capacity_bytes`,
// `available_bytes`, `reported_at`) are intentionally absent and also
// rejected by the key sweep.
type createRequest struct {
	Node   string          `json:"node"`
	Name   string          `json:"name"`
	Type   string          `json:"type"`
	Path   string          `json:"path"`
	Config json.RawMessage `json:"config,omitempty"`
}

// updateRequest is the body of PATCH /v1/storage-pools/{id}. Mirrors
// StoragePoolUpdate. Field-level semantics:
//
//   - Name: nil pointer = leave as-is, non-nil = set.
//   - Config: plain (non-pointer) RawMessage; length 0 means the key
//     was absent, anything non-empty gets validated as a JSON object
//     and persisted. `null` is rejected — the column is jsonb NOT NULL
//     with a `'{}'` default.
//
// `node`, `type`, `path`, and `is_default` are intentionally ABSENT
// from the struct. The handler does a pre-decode key sweep that rejects
// each of them with 400 forbidden_fields. The agent-reported keys
// (`capacity_bytes`, `available_bytes`, `reported_at`) are rejected by
// the same sweep.
type updateRequest struct {
	Name   *string         `json:"name,omitempty"`
	Config json.RawMessage `json:"config,omitempty"`
}

// listResponse is the payload for GET /v1/storage-pools.
type listResponse struct {
	Data []storagePoolView `json:"data"`
	Meta paginationMeta    `json:"meta"`
}

type paginationMeta struct {
	NextCursor *string `json:"next_cursor"`
}

// storageImageView mirrors components/schemas/StorageImage in
// api/openapi/control-plane.yaml. The referenced template and pool
// are surfaced as their names (operator-facing identifiers rather
// than UUIDs). The on-disk row's UUIDs remain primary keys; the
// wire shape just renders names.
type storageImageView struct {
	ID             string `json:"id"`
	Template       string `json:"template"`
	Pool           string `json:"pool"`
	ChecksumSHA256 string `json:"checksum_sha256"`
	SizeBytes      int64  `json:"size_bytes"`
	Format         string `json:"format"`
	ImportedAt     string `json:"imported_at"`
}

// storageImageListResponse is the payload for
// GET /v1/storage-pools/{pool_id}/images.
type storageImageListResponse struct {
	Data []storageImageView `json:"data"`
	Meta paginationMeta     `json:"meta"`
}
