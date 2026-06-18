// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cluster

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// GetDefaultArtifactPool implements GET /v1/cluster/default-artifact-pool.
// Returns the current default artifact pool name when set; 404
// default_artifact_pool_not_set when unconfigured. Permission: cluster:read.
func (h *Handler) GetDefaultArtifactPool(w http.ResponseWriter, r *http.Request) {
	settings, err := h.store.ClusterSettings(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "load cluster settings", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load cluster settings", nil)
		return
	}
	if settings.DefaultArtifactPoolName == nil {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeDefaultArtifactPoolNotSet,
			"cluster default artifact pool is not set", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, DefaultPool{Name: *settings.DefaultArtifactPoolName})
}

// SetDefaultArtifactPool implements PUT /v1/cluster/default-artifact-pool.
// Validates the requested name resolves to an existing artifact pool, then
// writes cluster_settings.default_artifact_pool_name. A disk pool name is not
// an artifact pool, so it is rejected with 400. Permission: cluster:manage.
func (h *Handler) SetDefaultArtifactPool(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "read request body", nil)
		return
	}
	var req setDefaultPoolRequest
	if err := json.Unmarshal(body, &req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "decode request body", nil)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "name is required", nil)
		return
	}

	ap, err := h.store.ArtifactPoolByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed,
				"no artifact pool with this name exists", nil)
			return
		}
		h.log.ErrorContext(r.Context(), "lookup artifact pool by name", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "validate artifact pool", nil)
		return
	}

	// Persist the canonical-casing variant the cluster already uses for this
	// pool (the resolver is case-insensitive, but storing the canonical form
	// keeps GET responses stable for operators who configured the value).
	canonical := ap.Name
	if err := h.store.SetDefaultArtifactPoolName(r.Context(), &canonical); err != nil {
		h.log.ErrorContext(r.Context(), "set cluster default artifact pool", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "persist default artifact pool", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, DefaultPool{Name: canonical})
}

// ClearDefaultArtifactPool implements DELETE /v1/cluster/default-artifact-pool.
// Idempotent - clearing an already-null value is a no-op 204. Permission:
// cluster:manage.
func (h *Handler) ClearDefaultArtifactPool(w http.ResponseWriter, r *http.Request) {
	if err := h.store.ClearDefaultArtifactPoolName(r.Context()); err != nil {
		h.log.ErrorContext(r.Context(), "clear cluster default artifact pool", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "clear default artifact pool", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
