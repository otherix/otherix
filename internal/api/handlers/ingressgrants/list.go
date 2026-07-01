// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ingressgrants

import (
	"net/http"

	"github.com/otherix/otherix/internal/api/pagination"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// List implements GET /v1/ingress-grants. Required permission: vm:ingress-grant.
// Cursor pagination per ADR 0019. A developer (scope=own) sees only the
// grants they created; admin/operator (scope=any) see all. The stored
// token hash is never surfaced.
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

	ownerScoped := auth.ScopeFor(caller.Role, auth.PermVMIngressGrant) == auth.ScopeOwn

	// Owner-scoped callers may have their own grants thinned out of a
	// page by the visibility filter below, so fetch in bounded batches
	// and keep advancing the store cursor until the page is full or the
	// collection is exhausted. Any-scope callers fill a page in one pass.
	limitN := int(limit)
	params := store.ListIngressGrantsParams{LimitCount: limit + 1}
	if cur != nil {
		params.CursorCreatedAt = &cur.CreatedAt
		params.CursorID = &cur.ID
	}

	views := make([]grantView, 0, limitN)
	var nextCursor *string
	for {
		rows, err := h.store.ListIngressGrants(r.Context(), params)
		if err != nil {
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "list ingress grants", nil)
			return
		}
		if len(rows) == 0 {
			break
		}

		exhausted := len(rows) <= limitN
		page := rows
		if !exhausted {
			page = rows[:limitN]
		}

		for _, g := range page {
			if ownerScoped && g.CreatedBy != caller.ID {
				continue
			}
			views = append(views, toView(g))
			if len(views) == limitN {
				c := pagination.Encode(&pagination.Cursor{CreatedAt: g.CreatedAt, ID: g.ID})
				nextCursor = &c
				break
			}
		}

		if nextCursor != nil || exhausted {
			break
		}
		// Advance the store cursor past the last row examined and refill.
		last := page[len(page)-1]
		params.CursorCreatedAt = &last.CreatedAt
		params.CursorID = &last.ID
	}

	response.WriteJSON(w, r, http.StatusOK, listResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	})
}
