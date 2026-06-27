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

// setDefaultNetworkRequest is the PUT body. Name must reference an existing
// bridge network; the handler validates this before writing cluster_settings.
type setDefaultNetworkRequest struct {
	Name string `json:"name"`
}

// GetDefaultNetwork implements GET /v1/cluster/default-network. Returns the
// current default network name when set; 404 default_network_not_set when
// unconfigured. Permission: cluster:read.
func (h *Handler) GetDefaultNetwork(w http.ResponseWriter, r *http.Request) {
	settings, err := h.store.ClusterSettings(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "load cluster settings", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load cluster settings", nil)
		return
	}
	if settings.DefaultNetworkName == nil {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeDefaultNetworkNotSet,
			"cluster default network is not set", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, DefaultPool{Name: *settings.DefaultNetworkName})
}

// SetDefaultNetwork implements PUT /v1/cluster/default-network. Validates the
// requested name resolves to an existing bridge network, then writes
// cluster_settings.default_network_name. A non-bridge or unknown name is
// rejected with 400. Permission: cluster:manage.
func (h *Handler) SetDefaultNetwork(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "read request body", nil)
		return
	}
	var req setDefaultNetworkRequest
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

	net, err := h.store.NetworkByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed,
				"no network with this name exists", nil)
			return
		}
		h.log.ErrorContext(r.Context(), "lookup network by name", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "validate network", nil)
		return
	}
	if net.Type != store.NetworkTypeBridge {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"default network must be a bridge network", nil)
		return
	}

	// Persist the canonical-casing variant the cluster holds for this
	// network (names are unique; storing canonical case keeps GET stable).
	canonical := net.Name
	if err := h.store.SetDefaultNetworkName(r.Context(), &canonical); err != nil {
		h.log.ErrorContext(r.Context(), "set cluster default network", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "persist default network", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, DefaultPool{Name: canonical})
}

// ClearDefaultNetwork implements DELETE /v1/cluster/default-network.
// Idempotent - clearing an already-null value is a no-op 204. Permission:
// cluster:manage.
func (h *Handler) ClearDefaultNetwork(w http.ResponseWriter, r *http.Request) {
	if err := h.store.ClearDefaultNetworkName(r.Context()); err != nil {
		h.log.ErrorContext(r.Context(), "clear cluster default network", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "clear default network", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
