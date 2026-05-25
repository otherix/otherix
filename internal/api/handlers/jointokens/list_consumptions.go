// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package jointokens

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/pagination"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// ListConsumptions implements GET /v1/nodes/join-tokens/{id}/consumptions.
// Required permission: node:manage. Cursor pagination.
// Returns empty data array in Step 1 — the redemption endpoint (which
// would populate join_token_consumptions) lands in Step 2.
func (h *Handler) ListConsumptions(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "id must be a uuid", nil)
		return
	}

	// Confirm the token exists so an unknown id surfaces as 404
	// regardless of consumption-row count.
	if _, err := h.loadToken(r.Context(), id); err != nil {
		if errors.Is(err, errTokenNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "join token not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load token", nil)
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

	params := store.ListJoinTokenConsumptionsParams{
		JoinTokenID: id,
		LimitCount:  limit + 1,
	}
	if cur != nil {
		params.CursorConsumedAt = &cur.CreatedAt
		params.CursorID = &cur.ID
	}

	rows, err := h.store.Queries().ListJoinTokenConsumptions(r.Context(), params)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list consumptions", nil)
		return
	}

	limitN := int(limit)
	var nextCursor *string
	if len(rows) > limitN {
		last := rows[limitN-1]
		s := pagination.Encode(&pagination.Cursor{CreatedAt: last.ConsumedAt, ID: last.ID})
		nextCursor = &s
		rows = rows[:limitN]
	}

	views := make([]consumptionView, 0, len(rows))
	for _, c := range rows {
		views = append(views, toConsumptionView(c))
	}

	response.WriteJSON(w, r, http.StatusOK, consumptionsListResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	})
}
