// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/pagination"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// listResponse is the cursor-list envelope: a data array plus pagination
// meta. Mirrors the shared list shape used by every list endpoint.
type listResponse struct {
	Data []migrationView `json:"data"`
	Meta paginationMeta  `json:"meta"`
}

type paginationMeta struct {
	NextCursor *string `json:"next_cursor"`
}

// List implements GET /v1/migrations. Required permission: vm:read. Cursor
// pagination newest-first by (created_at, id); optional ?vm= (migrations for a
// VM) and ?node= (migrations touching a node, source or target) UUID filters.
//
// A ScopeOwn vm:read caller (none in the current matrix; kept for forward-
// compat) sees only migrations whose owning VM they own - foreign migrations
// are dropped from the page so existence is not leaked. next_cursor is derived
// from the last row the store returned (pre-ownership-filter) so paging stays
// monotonic and terminates.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	q := r.URL.Query()
	limit := pagination.Limit(pagination.ParseLimit(q.Get("limit")))
	cur, err := pagination.Decode(q.Get("cursor"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid cursor", nil)
		return
	}

	params := store.ListMigrationsParams{LimitCount: limit + 1}
	if raw := q.Get("vm"); raw != "" {
		id, perr := uuid.Parse(raw)
		if perr != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "vm must be a uuid", nil)
			return
		}
		params.VmID = &id
	}
	if raw := q.Get("node"); raw != "" {
		id, perr := uuid.Parse(raw)
		if perr != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "node must be a uuid", nil)
			return
		}
		params.NodeID = &id
	}
	if cur != nil {
		params.CursorCreatedAt = &cur.CreatedAt
		params.CursorID = &cur.ID
	}

	rows, err := h.store.ListMigrations(r.Context(), params)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list migrations", nil)
		return
	}

	limitN := int(limit)
	var nextCursor *string
	if len(rows) > limitN {
		last := rows[limitN-1]
		s := pagination.Encode(&pagination.Cursor{CreatedAt: last.CreatedAt, ID: last.ID})
		nextCursor = &s
		rows = rows[:limitN]
	}

	scope := auth.ScopeFor(caller.Role, auth.PermVMRead)
	views := make([]migrationView, 0, len(rows))
	for _, m := range rows {
		if scope == auth.ScopeOwn && !h.callerOwnsMigration(r, caller, m) {
			continue
		}
		views = append(views, h.viewWithNames(r.Context(), m))
	}

	response.WriteJSON(w, r, http.StatusOK, listResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	})
}

// callerOwnsMigration reports whether caller owns the VM the migration is for.
// A failed VM load is treated as not-owned (no-leak fall-through).
func (h *Handler) callerOwnsMigration(r *http.Request, caller *auth.User, m store.Migration) bool {
	vm, err := h.store.VMByID(r.Context(), m.VmID)
	if err != nil {
		return false
	}
	return vm.OwnerID == caller.ID
}
