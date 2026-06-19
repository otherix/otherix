// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshots

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Get implements GET /v1/snapshots/{id}. Required permission: snapshot:read. The
// {id} param is a full snapshot UUID. Ownership is checked against the
// snapshot's owner_id (which mirrors the parent VM owner): a non-owner gets 404,
// shared with the genuinely-absent envelope so existence is not leaked.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "id must be a uuid", nil)
		return
	}

	snap, err := h.store.SnapshotByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeSnapshotNotFound(w, r)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load snapshot", nil)
		return
	}

	if err := auth.CheckOwnership(caller, &snap.OwnerID, auth.PermSnapshotRead); err != nil {
		writeSnapshotNotFound(w, r)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, h.viewWithDurability(r.Context(), snap))
}
