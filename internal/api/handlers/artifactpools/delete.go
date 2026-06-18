// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package artifactpools

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// Delete implements DELETE /v1/artifact-pools/{id} (UUID or name). Required
// permission: storage_pool:manage. Refuses with 409 + blocking_resources while
// any non-deleted snapshot references the pool; no force-delete.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
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
	if err := h.store.DeleteArtifactPool(r.Context(), ap.ID); err != nil {
		var inUse *store.ResourceInUseError
		switch {
		case errors.Is(err, store.ErrNotFound):
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "artifact pool not found", nil)
		case errors.As(err, &inUse):
			response.WriteBlockingResources(w, r, &response.BlockingResourcesError{
				Message:   "artifact pool is referenced by snapshots; delete them first",
				Resources: inUse.Resources,
			})
		default:
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "delete artifact pool", nil)
		}
		return
	}
	response.WriteNoContent(w)
}
