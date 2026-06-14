// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Cancel implements POST /v1/migrations/{id}/cancel - sync (200) with the
// current Migration view. Best-effort and valid only pre-cutover (spec D5):
// PinnedNodeID is never touched, so a cancel is trivially fail-safe-to-source.
// Required permission: vm:migrate (a cancel mutates the migration, same gate as
// initiating it). A developer cancelling another owner's migration gets 404
// (no existence leak).
//
// An already-terminal migration (completed / failed / cancelled) is returned
// unchanged at 200 - cancel is idempotent and best-effort, mirroring the
// cordon/uncordon no-op precedent. Genuinely missing migrations return 404.
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
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

	m, err := h.store.MigrationByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeMigrationNotFound(w, r)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load migration", nil)
		return
	}

	// Ownership gate on vm:migrate: load the owning VM, deny cross-owner with
	// 404 (no leak). vm:migrate is operator+ (ScopeAny) in the matrix, so the
	// VM load is the only cross-owner branch in practice.
	vm, err := h.store.VMByID(r.Context(), m.VmID)
	if err != nil {
		// The owning VM vanished (force-delete race): treat as invisible.
		writeMigrationNotFound(w, r)
		return
	}
	if err := auth.CheckOwnership(caller, &vm.OwnerID, auth.PermVMMigrate); err != nil {
		writeMigrationNotFound(w, r)
		return
	}

	updated, err := h.store.CancelMigration(r.Context(), id, "cancelled by "+caller.ID.String())
	if err != nil {
		if errors.Is(err, store.ErrMigrationNotCancelable) {
			// Already terminal: best-effort, return the current row unchanged at 200.
			response.WriteJSON(w, r, http.StatusOK, toView(updated))
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeMigrationNotFound(w, r)
			return
		}
		h.log.ErrorContext(r.Context(), "migrations.cancel failed",
			"migration_id", id.String(), "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "cancel migration", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, toView(updated))
}
