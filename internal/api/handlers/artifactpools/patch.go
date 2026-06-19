// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactpools

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// Patch implements PATCH /v1/artifact-pools/{id} (UUID or name; the name is
// immutable). Required permission: storage_pool:manage. It updates the pool's
// replication_factor and/or advisory membership; at least one of the two must be
// present.
//
// replication_factor is live-effective: increasing it immediately drives
// re-replication up to the new target through the existing durability reconcile
// loop, which reads the pool's current replication_factor each pass. Decreasing
// it is allowed and leaves the now-excess copies in place until a future
// garbage-collection pass reclaims them - durability stays correct because the
// number of copies stays at or above the new target, never below it.
func (h *Handler) Patch(w http.ResponseWriter, r *http.Request) {
	var req patchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}
	if req.ReplicationFactor == nil && req.Membership == nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "no fields to update", nil)
		return
	}

	ap, err := h.resolve(r, chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "artifact pool not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load artifact pool", nil)
		return
	}

	params, msg := buildPatchParams(ap, &req)
	if msg != "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, msg, nil)
		return
	}

	updated, err := h.store.UpdateArtifactPool(r.Context(), ap.ID, params)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "artifact pool not found", nil)
			return
		}
		h.log.ErrorContext(r.Context(), "update artifact pool", "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "update artifact pool", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, toView(updated))
}

// buildPatchParams merges req onto the existing pool, validating the effective
// values. It returns the store params for the fields actually being changed, or
// a non-empty validation message (with zero params) when a value is rejected.
func buildPatchParams(ap store.ArtifactPool, req *patchRequest) (store.UpdateArtifactPoolParams, string) {
	// Effective values: the patched field when present, else the existing row.
	effectiveRF := ap.ReplicationFactor
	if req.ReplicationFactor != nil {
		effectiveRF = *req.ReplicationFactor
		if err := validation.ValidateReplicationFactor(effectiveRF); err != nil {
			return store.UpdateArtifactPoolParams{}, err.Error()
		}
	}
	effectiveMembership := ap.Membership
	if req.Membership != nil {
		effectiveMembership = store.ArtifactPoolMembership{
			AllNodes: req.Membership.AllNodes,
			Nodes:    req.Membership.Nodes,
		}
	}

	// A concrete replication_factor cannot exceed an explicit member list. When
	// membership is "all nodes" the eligible set is dynamic, so there is no
	// static cap to enforce.
	if !effectiveMembership.AllNodes && !effectiveRF.All &&
		int(effectiveRF.Count) > len(effectiveMembership.Nodes) {
		return store.UpdateArtifactPoolParams{}, "replication_factor exceeds the number of pool members"
	}

	var params store.UpdateArtifactPoolParams
	if req.ReplicationFactor != nil {
		params.ReplicationFactor = &effectiveRF
	}
	if req.Membership != nil {
		params.Membership = &effectiveMembership
	}
	return params, ""
}
