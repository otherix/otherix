// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package users

import (
	"errors"
	"net/http"

	"github.com/otherix/otherix/internal/api/pagination"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// List implements GET /v1/users. Required permission: user:read
// (admin / operator). Supports cursor pagination plus an exact-match
// `email` query filter.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if email := r.URL.Query().Get("email"); email != "" {
		h.listByEmail(w, r, email)
		return
	}

	limit := pagination.Limit(pagination.ParseLimit(r.URL.Query().Get("limit")))
	cur, err := pagination.Decode(r.URL.Query().Get("cursor"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid cursor", nil)
		return
	}

	params := store.ListUsersParams{LimitCount: limit + 1}
	if cur != nil {
		params.CursorCreatedAt = &cur.CreatedAt
		params.CursorID = &cur.ID
	}

	rows, err := h.store.ListUsers(r.Context(), params)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list users", nil)
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

	views := make([]userView, 0, len(rows))
	for _, u := range rows {
		views = append(views, toView(u))
	}
	response.WriteJSON(w, r, http.StatusOK, listResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	})
}

// listByEmail handles the exact-match filter. The (lower-cased) email
// column is unique among non-deleted rows, so the result is at most
// one user. A miss returns an empty page rather than 404.
func (h *Handler) listByEmail(w http.ResponseWriter, r *http.Request, email string) {
	row, err := h.store.UserByEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteJSON(w, r, http.StatusOK, listResponse{
				Data: []userView{},
				Meta: paginationMeta{},
			})
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "lookup by email", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, listResponse{
		Data: []userView{toView(row)},
		Meta: paginationMeta{},
	})
}
