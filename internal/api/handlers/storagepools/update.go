// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// forbiddenUpdateKeys lists the keys PATCH /v1/storage-pools/{id}
// rejects with 400 forbidden_fields. `node` / `type` / `path` are
// API-immutable; the agent-reported usage fields are out-of-band.
// `is_default` is rejected because there is no per-row
// default flag - cluster default-pool lives in cluster_settings.
// The legacy `node_id` spelling is kept in the forbidden list so an
// older client sees a clear envelope rather than a silently-ignored
// field. Keep alphabetised.
var forbiddenUpdateKeys = []string{
	"available_bytes",
	"capacity_bytes",
	"is_default",
	"node",
	"node_id",
	"path",
	"reported_at",
	"type",
}

// Update implements PATCH /v1/storage-pools/{id}. Required permission:
// storage_pool:manage (admin only). {id} accepts either a UUID literal
// or a pool name. The handler does a two-step decode: it first
// inspects the raw JSON keys to reject API-immutable and
// agent-reported fields with 400 forbidden_fields, then merges the
// typed updateRequest into the existing row.
//
// Name resolution by bare name fails with 400 pool_name_ambiguous
// when multiple instances share the name - callers must address a
// specific instance by UUID.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	row, err := resolver.Pool(r.Context(), h.store.Queries(), chi.URLParam(r, "id"))
	if err != nil {
		writePoolResolveError(w, r, err, "storage pool not found", "load storage pool")
		return
	}
	req, ok := decodeUpdateRequest(w, r)
	if !ok {
		return
	}

	if !applyUpdate(w, r, &row, &req) {
		return
	}

	updated, err := h.store.Queries().UpdateStoragePool(r.Context(), store.UpdateStoragePoolParams{
		ID:     row.ID,
		Name:   row.Name,
		Config: row.Config,
	})
	if err != nil {
		writeUpdateError(w, r, err)
		return
	}

	view, err := h.projectView(r.Context(), updated)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "project storage pool view", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, view)
}

// decodeUpdateRequest reads the raw body, screens out
// API-immutable / agent-reported fields, and decodes the typed
// updateRequest. ok=false signals the response has already been
// written.
func decodeUpdateRequest(w http.ResponseWriter, r *http.Request) (updateRequest, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "could not read body", nil)
		return updateRequest{}, false
	}

	if forbidden := validation.ScanForbiddenKeys(body, forbiddenUpdateKeys); len(forbidden) > 0 {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"one or more fields cannot be modified",
			map[string]any{"forbidden_fields": forbidden})
		return updateRequest{}, false
	}

	var req updateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return updateRequest{}, false
	}
	return req, true
}

// writeUpdateError maps the post-UPDATE database error to the standard
// envelope. The 23505 unique-violation maps to the per-node scope of
// uq_storage_pools_name.
func writeUpdateError(w http.ResponseWriter, r *http.Request, err error) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict,
			"another storage pool with this name already exists on the target node",
			map[string]any{"field": "name"})
		return
	}
	response.WriteError(w, r, http.StatusInternalServerError,
		response.CodeInternal, "update storage pool", nil)
}

// applyUpdate merges req into row in place. Returns false (after
// writing the validation-failed response) when a field is invalid.
func applyUpdate(w http.ResponseWriter, r *http.Request, row *store.StoragePool, req *updateRequest) bool {
	if req.Name != nil {
		if err := validation.ValidateStoragePoolName(*req.Name); err != nil {
			writeValidation(w, r, err.Error())
			return false
		}
		row.Name = *req.Name
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

// writeValidation is a thin shorthand around the validation_failed
// envelope used by applyUpdate.
func writeValidation(w http.ResponseWriter, r *http.Request, msg string) {
	response.WriteError(w, r, http.StatusBadRequest,
		response.CodeValidationFailed, msg, nil)
}

// writePoolResolveError centralises the wire mapping of resolver.Pool
// failures used by every handler in the package (Update / Delete /
// Get-by-UUID / Scan / Image*). The multi-instance model needs
// CodeAmbiguous, which surfaces as 400 pool_name_ambiguous - callers
// must address the per-node instance by UUID.
func writePoolResolveError(w http.ResponseWriter, r *http.Request, err error, notFoundMsg, internalMsg string) {
	if resolver.IsNotFound(err) {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, notFoundMsg, nil)
		return
	}
	var rerr *resolver.Error
	if errors.As(err, &rerr) && rerr.Code == resolver.CodeAmbiguous {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodePoolNameAmbiguous,
			"pool name resolves to multiple instances; address by UUID",
			map[string]any{"name": rerr.Identifier})
		return
	}
	response.WriteError(w, r, http.StatusInternalServerError,
		response.CodeInternal, internalMsg, nil)
}
