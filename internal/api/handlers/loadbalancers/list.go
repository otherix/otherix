// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import (
	"net/http"

	"github.com/google/uuid"

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

	ctx := r.Context()
	// Memoize the owner's VM scan across the page: N load balancers sharing an
	// owner scan that owner's VMs once. This mirrors the same in-request memo the
	// connect/backend path uses. Cost note: ListVMsByOwner is a full per-owner
	// scan; a fast-follow owner-indexed lookup would drop it. A nil entry marks a
	// fetch that failed for that owner, so its load balancers get no summary.
	vmsByOwner := map[uuid.UUID][]store.VM{}
	views := make([]loadBalancerView, 0, len(rows))
	for _, lb := range rows {
		view := toView(lb)
		vms, ok := vmsByOwner[lb.OwnerID]
		if !ok {
			var err error
			vms, err = h.store.ListVMsByOwner(ctx, lb.OwnerID)
			if err != nil {
				// Best-effort per owner: skip the summary for this load balancer
				// (Health stays nil) rather than failing the whole list.
				h.log.WarnContext(ctx, "list owner vms for lb health summary",
					"loadbalancer_id", lb.ID.String(), "error", err.Error())
				vms = nil
			}
			vmsByOwner[lb.OwnerID] = vms
		}
		if vms != nil {
			view.Health = summarizeBackends(h.buildBackends(ctx, lb, vms))
		}
		views = append(views, view)
	}
	response.WriteJSON(w, r, http.StatusOK, listResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	})
}
