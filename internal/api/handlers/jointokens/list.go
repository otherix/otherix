// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package jointokens

import (
	"net/http"

	"github.com/otherix/otherix/internal/api/pagination"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// List implements GET /v1/nodes/join-tokens. Required permission:
// node:manage (admin-only). Cursor pagination;
// include_expired=true to surface expired rows (default false).
//
// consumption_count surfaces via correlated subquery in the sqlc-
// generated query — first slice has low cardinality so the
// non-index'd subquery is acceptable.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := pagination.Limit(pagination.ParseLimit(q.Get("limit")))

	cur, err := pagination.Decode(q.Get("cursor"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid cursor", nil)
		return
	}

	includeExpired := q.Get("include_expired") == "true"

	params := store.ListJoinTokensParams{
		IncludeExpired: includeExpired,
		LimitCount:     limit + 1, // +1 row to detect next page.
	}
	if cur != nil {
		params.CursorCreatedAt = &cur.CreatedAt
		params.CursorID = &cur.ID
	}

	rows, err := h.store.Queries().ListJoinTokens(r.Context(), params)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list join tokens", nil)
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

	views := make([]joinTokenView, 0, len(rows))
	for _, row := range rows {
		views = append(views, toViewFromListRow(row))
	}

	response.WriteJSON(w, r, http.StatusOK, listResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	})
}
