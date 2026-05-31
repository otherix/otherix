// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cluster

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/otherix/otherix/internal/api/response"
)

// DefaultPool is the wire shape of the cluster default-pool reference.
// Mirrors api/openapi/control-plane.yaml#components/schemas/ClusterDefaultPool.
// The type stays unprefixed inside the cluster package so external
// callers spell it `cluster.DefaultPool` rather than the stuttering
// `cluster.ClusterDefaultPool`.
type DefaultPool struct {
	Name string `json:"name"`
}

// setDefaultPoolRequest is the PUT body. Name must reference an
// existing pool name (at least one instance across the cluster); the
// handler validates this before writing to cluster_settings.
type setDefaultPoolRequest struct {
	Name string `json:"name"`
}

// GetDefaultPool implements GET /v1/cluster/default-pool. Returns the
// current default pool name when set; 404 default_pool_not_set when
// unconfigured. Permission: cluster:read.
func (h *Handler) GetDefaultPool(w http.ResponseWriter, r *http.Request) {
	settings, err := h.store.ClusterSettings(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "load cluster settings", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load cluster settings", nil)
		return
	}
	if settings.DefaultPoolName == nil {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeDefaultPoolNotSet,
			"cluster default pool is not set", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, DefaultPool{Name: *settings.DefaultPoolName})
}

// SetDefaultPool implements PUT /v1/cluster/default-pool. Validates
// the requested name resolves to at least one existing pool instance,
// then writes to cluster_settings.default_pool_name. Permission:
// cluster:manage.
func (h *Handler) SetDefaultPool(w http.ResponseWriter, r *http.Request) {
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

	rows, err := h.store.StoragePoolsByName(r.Context(), name)
	if err != nil {
		h.log.ErrorContext(r.Context(), "list pools by name", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "validate pool name", nil)
		return
	}
	if len(rows) == 0 {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodePoolNotFound,
			"no pool instance with this name exists in the cluster", nil)
		return
	}

	// Persist the canonical-casing variant the cluster already uses
	// for this pool (resolver is case-insensitive but storing the
	// canonical form keeps GET responses stable for operators who
	// configured the value).
	canonical := rows[0].Name
	if err := h.store.SetDefaultPoolName(r.Context(), &canonical); err != nil {
		h.log.ErrorContext(r.Context(), "set cluster default pool", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "persist default pool", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, DefaultPool{Name: canonical})
}

// ClearDefaultPool implements DELETE /v1/cluster/default-pool. Idempotent
// — clearing an already-null value is a no-op 204. Permission:
// cluster:manage.
func (h *Handler) ClearDefaultPool(w http.ResponseWriter, r *http.Request) {
	// ClearDefaultPoolName is an idempotent UPDATE on the migration-seeded
	// singleton, so it never reports a missing row; any error here is a
	// genuine database fault.
	if err := h.store.ClearDefaultPoolName(r.Context()); err != nil {
		h.log.ErrorContext(r.Context(), "clear cluster default pool", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "clear default pool", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
