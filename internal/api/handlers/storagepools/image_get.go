// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// GetImage implements GET /v1/storage-pools/{pool_id}/images/{image_id}.
// Required permission: `image_cache:read`. {pool_id} accepts either a
// UUID literal or a pool name; {image_id} stays UUID-only since
// storage_images carry no name. Cross-pool addressing - an image_id
// that exists but lives in a different pool - yields 404 by the same
// no-existence-leak rule documented in CLAUDE.md. Genuinely-missing
// rows return the same 404 envelope so callers cannot distinguish
// "exists but in a different pool" from "does not exist".
func (h *Handler) GetImage(w http.ResponseWriter, r *http.Request) {
	pool, err := resolver.Pool(r.Context(), h.store, chi.URLParam(r, "pool_id"))
	if err != nil {
		writePoolResolveError(w, r, err, "storage pool not found", "load storage pool")
		return
	}

	imageID, err := uuid.Parse(chi.URLParam(r, "image_id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "image_id must be a uuid", nil)
		return
	}

	row, err := h.store.StorageImageByID(r.Context(), imageID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeImageNotFound(w, r)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load storage image", nil)
		return
	}

	if row.PoolID != pool.ID {
		writeImageNotFound(w, r)
		return
	}

	view, err := h.projectImageView(r.Context(), row, pool.Name)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "project storage image", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, view)
}

func writeImageNotFound(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusNotFound,
		response.CodeNotFound, "storage image not found", nil)
}
