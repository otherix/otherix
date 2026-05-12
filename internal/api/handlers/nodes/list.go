// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import (
	"net/http"

	"github.com/otherix/otherix/internal/api/pagination"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// List implements GET /v1/nodes. Required permission: node:read.
// admin / operator receive nodeView entries; developer / viewer
// receive nodeSummaryView entries. Cursor pagination,
// optional architecture / status query filters.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := pagination.Limit(pagination.ParseLimit(q.Get("limit")))
	cur, err := pagination.Decode(q.Get("cursor"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid cursor", nil)
		return
	}

	params := store.ListNodesEffectiveParams{LimitCount: limit + 1}
	if arch := q.Get("architecture"); arch != "" {
		switch arch {
		case string(store.CpuArchAmd64), string(store.CpuArchArm64):
			a := store.CpuArch(arch)
			params.Architecture = &a
		default:
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "invalid architecture", nil)
			return
		}
	}
	if status := q.Get("status"); status != "" {
		if !validNodeStatus(status) {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "invalid status", nil)
			return
		}
		s := store.NodeStatus(status)
		params.Status = &s
	}
	if cur != nil {
		params.CursorCreatedAt = &cur.CreatedAt
		params.CursorID = &cur.ID
	}

	rows, err := h.store.Queries().ListNodesEffective(r.Context(), params)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list nodes", nil)
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

	user := auth.UserFromContext(r.Context())
	full := user != nil && (user.Role == auth.RoleAdmin || user.Role == auth.RoleOperator)

	views := make([]any, 0, len(rows))
	for _, n := range rows {
		if full {
			views = append(views, toViewEffective(n))
		} else {
			views = append(views, toSummaryViewEffective(n))
		}
	}
	response.WriteJSON(w, r, http.StatusOK, listResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	})
}

func validNodeStatus(s string) bool {
	switch store.NodeStatus(s) {
	case store.NodeStatusPending,
		store.NodeStatusReady,
		store.NodeStatusCordoned,
		store.NodeStatusDraining,
		store.NodeStatusUnreachable,
		store.NodeStatusGone:
		return true
	}
	return false
}
