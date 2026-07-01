// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import (
	"net/http"

	"github.com/otherix/otherix/internal/api/pagination"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// List implements GET /v1/loadbalancers. Required permission:
// loadbalancer:read. Cursor pagination.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := pagination.Limit(pagination.ParseLimit(q.Get("limit")))
	cur, err := pagination.Decode(q.Get("cursor"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid cursor", nil)
		return
	}

	params := store.ListLoadBalancersParams{LimitCount: limit + 1}
	if cur != nil {
		params.CursorCreatedAt = &cur.CreatedAt
		params.CursorID = &cur.ID
	}

	rows, err := h.store.ListLoadBalancers(r.Context(), params)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list load balancers", nil)
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

	views := make([]loadBalancerView, 0, len(rows))
	for _, lb := range rows {
		views = append(views, toView(lb))
	}
	response.WriteJSON(w, r, http.StatusOK, listResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	})
}
