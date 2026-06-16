// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import (
	"errors"
	"net/http"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Cancel implements POST /v1/migrations/{id}/cancel - sync (200) with the
// current Migration view. Best-effort and valid only pre-cutover (spec D5):
// PinnedNodeID is never touched, so a cancel is trivially fail-safe-to-source.
// Required permission: vm:migrate (a cancel mutates the migration, same gate as
// initiating it), held only by admin and operator (both scope any); developer
// and viewer have no vm:migrate and are rejected by RequirePermission (403)
// before this handler runs. A caller who can cancel but cannot otherwise see the
// migration gets 404 (no existence leak).
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

	m, ok := h.resolveMigration(w, r)
	if !ok {
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

	// Record WHO cancelled in a human-readable form: the caller's display name,
	// not the opaque principal UUID (JWTs carry only the user id, so resolve it)
	// and not the email (PII that would be exposed on the migration record to
	// every reader). Fall back to the id when the display name is unset or the
	// user vanished (soft-deleted mid-request) - the cancel must not fail on a
	// cosmetic lookup.
	cancelledBy := caller.ID.String()
	if u, uerr := h.store.UserByID(r.Context(), caller.ID); uerr == nil && u.DisplayName != "" {
		cancelledBy = u.DisplayName
	}
	updated, err := h.store.CancelMigration(r.Context(), m.ID, "cancelled by "+cancelledBy)
	if err != nil {
		if errors.Is(err, store.ErrMigrationNotCancelable) {
			// Already terminal: best-effort, return the current row unchanged at 200.
			response.WriteJSON(w, r, http.StatusOK, h.viewWithNames(r.Context(), updated))
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeMigrationNotFound(w, r)
			return
		}
		h.log.ErrorContext(r.Context(), "migrations.cancel failed",
			"migration_id", m.ID.String(), "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "cancel migration", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, h.viewWithNames(r.Context(), updated))
}
