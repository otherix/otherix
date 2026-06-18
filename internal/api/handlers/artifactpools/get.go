// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactpools

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// Get implements GET /v1/artifact-pools/{id} (UUID or name; names are globally
// unique). Required permission: storage_pool:read.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
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
	response.WriteJSON(w, r, http.StatusOK, toView(ap))
}

func (h *Handler) resolve(r *http.Request, identifier string) (store.ArtifactPool, error) {
	if id, err := uuid.Parse(identifier); err == nil {
		return h.store.ArtifactPoolByID(r.Context(), id)
	}
	return h.store.ArtifactPoolByName(r.Context(), identifier)
}
