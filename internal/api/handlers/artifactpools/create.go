// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactpools

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// Create implements POST /v1/artifact-pools. Required permission:
// storage_pool:manage.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}
	if err := validation.ValidateArtifactPoolName(req.Name); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}
	if err := validation.ValidateReplicationFactor(req.ReplicationFactor); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}

	membership := store.ArtifactPoolMembership{AllNodes: true}
	if req.Membership != nil {
		membership = store.ArtifactPoolMembership{AllNodes: req.Membership.AllNodes, Nodes: req.Membership.Nodes}
	}

	ap, err := h.store.CreateArtifactPool(r.Context(), store.CreateArtifactPoolParams{
		ID:                uuid.New(),
		Name:              req.Name,
		ReplicationFactor: req.ReplicationFactor,
		Membership:        membership,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrArtifactPoolNameExists):
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "an artifact pool with this name already exists", nil)
		case errors.Is(err, store.ErrPoolNameConflict):
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "name is already used by a storage pool", nil)
		default:
			h.log.ErrorContext(r.Context(), "create artifact pool", "error", err)
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "create artifact pool", nil)
		}
		return
	}
	response.WriteJSON(w, r, http.StatusCreated, toView(ap))
}
