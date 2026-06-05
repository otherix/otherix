// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package users

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Delete implements DELETE /v1/users/{id}. Required permission:
// user:manage (admin only). Soft-deletes the user, revokes every
// active API token they own, and fails with 409 conflict when the
// user still owns vms / templates / snapshots — those FKs use ON
// DELETE RESTRICT and would block the delete anyway, so
// the handler returns a typed 409 with per-resource counts before
// hitting the database.
//
// Self-deletion is rejected with 400 to prevent operator lockout.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
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
	if id == caller.ID {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "caller may not delete themselves", nil)
		return
	}

	if _, err := h.store.UserByID(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "user not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load user", nil)
		return
	}

	counts, err := h.store.CountUserResources(r.Context(), id)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "count owned resources", nil)
		return
	}
	if counts.Vms+counts.Snapshots > 0 {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict,
			"user still owns one or more resources",
			map[string]any{
				"blocking_resources": map[string]int64{
					"vms":          counts.Vms,
					"vm_snapshots": counts.Snapshots,
				},
			})
		return
	}

	if err := h.store.DeleteUser(r.Context(), id); err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete user", nil)
		return
	}

	response.WriteNoContent(w)
}
