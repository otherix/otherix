// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// forbiddenCreateKeys is the set of keys that POST /v1/storage-pools
// rejects with 400 forbidden_fields. Agent-reported usage fields are
// out-of-band; `is_default` is also rejected -
// cluster default-pool is configured via
// PUT /v1/cluster/default-pool, not per-instance. Keep alphabetised.
var forbiddenCreateKeys = []string{
	"available_bytes",
	"capacity_bytes",
	"is_default",
	"reported_at",
}

// Create implements POST /v1/storage-pools. Required permission:
// storage_pool:manage (admin only). Returns 201 with the projected
// StoragePool on success.
//
// The request body's `node` field is a node name; UUID literals are
// rejected with 400 validation_failed at the resolver. 404 not_found
// is returned when the referenced node is missing or soft-deleted
// (never leak existence with a 4xx distinction). 409 conflict is
// returned for duplicate pool name on the same node - multiple nodes
// may host the same pool name under the multi-instance model. Default-
// pool selection has moved to the cluster_settings endpoint and no
// longer collides with create.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeCreateRequest(w, r)
	if !ok {
		return
	}

	// Path allowlist gate. Runs after the syntactic
	// path check (decodeCreateRequest → validateCreateFields →
	// ValidatePoolPath) so the dedicated path_not_allowed envelope only
	// fires when the path is well-formed but points outside the
	// operator-configured prefixes.
	if err := validation.ValidatePoolPathAgainstAllowlist(req.Path, h.cfg.AllowedPathPrefixes); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodePathNotAllowed,
			"path is not on the storage_pools allowlist",
			map[string]any{
				"allowed_prefixes": h.cfg.AllowedPathPrefixes,
				"path":             req.Path,
			})
		return
	}

	node, err := resolver.Node(r.Context(), h.store.Queries(), req.Node)
	if err != nil {
		if resolver.IsUUIDInName(err) {
			response.WriteUUIDNotAllowedError(w, r, "node", "node")
			return
		}
		if resolver.IsNotFound(err) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "node not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load node", nil)
		return
	}

	row, err := h.store.Queries().CreateStoragePool(r.Context(), store.CreateStoragePoolParams{
		ID:     uuid.New(),
		NodeID: node.ID,
		Name:   req.Name,
		Type:   req.Type,
		Path:   req.Path,
		Config: normaliseConfig(req.Config),
	})
	if err != nil {
		writeCreateError(w, r, err)
		return
	}

	view, err := h.projectView(r.Context(), row)
	if err != nil {
		h.log.ErrorContext(r.Context(), "project storage pool view", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "project storage pool", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusCreated, view)
}

// decodeCreateRequest reads, sanitises, and validates the POST body.
// It returns the typed request; ok=false signals the response has
// already been written. The `node` field stays a raw string — the
// caller hands it to the resolver, which decides UUID vs name.
func decodeCreateRequest(w http.ResponseWriter, r *http.Request) (createRequest, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "could not read body", nil)
		return createRequest{}, false
	}

	if forbidden := validation.ScanForbiddenKeys(body, forbiddenCreateKeys); len(forbidden) > 0 {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"agent-reported fields are not accepted",
			map[string]any{"forbidden_fields": forbidden})
		return createRequest{}, false
	}

	var req createRequest
	if err := json.Unmarshal(body, &req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return createRequest{}, false
	}

	if req.Node == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "node is required", nil)
		return createRequest{}, false
	}

	if err := validateCreateFields(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return createRequest{}, false
	}

	return req, true
}

// validateCreateFields runs the API-edge validators in dependency
// order: name, type, path, config. The first failure short-circuits.
// Allowlist enforcement runs separately in the handler so the dedicated
// `path_not_allowed` error code stays distinguishable from generic
// `validation_failed`.
func validateCreateFields(req *createRequest) error {
	if err := validation.ValidateStoragePoolName(req.Name); err != nil {
		return err
	}
	if err := validation.ValidateStoragePoolType(req.Type); err != nil {
		return err
	}
	if err := validation.ValidatePoolPath(req.Path); err != nil {
		return err
	}
	return validateConfigShape(req.Config)
}

// writeCreateError maps the post-Insert error returned by the
// database to the standard envelope. The 23505 unique-violation maps
// to uq_storage_pools_name's per-node scope: two pools may share a
// name across nodes, but never on the same node.
func writeCreateError(w http.ResponseWriter, r *http.Request, err error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict,
			"another storage pool with this name already exists on the target node",
			map[string]any{"field": "name"})
		return
	}
	response.WriteError(w, r, http.StatusInternalServerError,
		response.CodeInternal, "persist storage pool", nil)
}

// validateConfigShape returns nil when the supplied bytes are either
// empty (caller omitted the field) or a JSON object literal.
// storage_pools.config is `jsonb NOT NULL DEFAULT '{}'`; arrays,
// scalars, and the `null` literal are rejected so the column never
// carries a shape downstream code does not expect.
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
// storage_pools.config column. An absent or empty payload becomes the
// canonical empty-object literal so the column never carries a NULL
// or empty string.
func normaliseConfig(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return []byte(raw)
}
