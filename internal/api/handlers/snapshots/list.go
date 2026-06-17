// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshots

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/pagination"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// listResponse is the cursor-list envelope: a data array plus pagination meta.
type listResponse struct {
	Data []vmSnapshotView `json:"data"`
	Meta paginationMeta   `json:"meta"`
}

type paginationMeta struct {
	NextCursor *string `json:"next_cursor"`
}

// List implements GET /v1/vms/{id}/snapshots. Required permission:
// snapshot:read. Cursor pagination newest-first by (created_at, id); optional
// ?status= filter. Ownership: the VM is resolved and ownership-checked first
// (404 on deny - no existence leak), so a non-owner cannot enumerate another
// owner's snapshots.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	vm, err := resolver.VM(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writeVMResolveError(w, r, err)
		return
	}
	if err := auth.CheckOwnership(caller, &vm.OwnerID, auth.PermSnapshotRead); err != nil {
		writeVMNotFound(w, r)
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

	vmID := vm.ID
	params := store.ListSnapshotsParams{VmID: &vmID, LimitCount: limit + 1}
	if raw := q.Get("status"); raw != "" {
		st := store.SnapshotStatus(raw)
		params.Status = &st
	}
	if cur != nil {
		params.CursorCreatedAt = &cur.CreatedAt
		params.CursorID = &cur.ID
	}

	rows, err := h.store.ListSnapshots(r.Context(), params)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list snapshots", nil)
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

	views := make([]vmSnapshotView, 0, len(rows))
	for _, s := range rows {
		views = append(views, toView(s))
	}

	response.WriteJSON(w, r, http.StatusOK, listResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	})
}
