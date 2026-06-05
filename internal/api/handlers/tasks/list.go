// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package tasks

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/pagination"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// List implements GET /v1/tasks. Required permission: `task:read`.
//
// RBAC scope dispatch:
//   - admin / operator (ScopeAny) → ListTasksAny.
//   - developer / viewer (ScopeOwn) → ListTasksOwn keyed on caller.ID.
//
// Cursor pagination with `(created_at, id)` keyed in DESC
// order. Optional ?status, ?type, ?resource_type, ?resource_id filters
// compose with AND (and with the RBAC clamp).
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

	filters, ok := parseListFilters(w, r, q)
	if !ok {
		return
	}

	rows, err := h.runList(r.Context(), caller, filters, cur, limit)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list tasks", nil)
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

	views := make([]taskView, 0, len(rows))
	for _, t := range rows {
		views = append(views, toView(t))
	}
	response.WriteJSON(w, r, http.StatusOK, listResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	})
}

// listFilters bundles the optional filter values once parsed and
// validated. Pointer-shaped because the underlying sqlc params accept
// nil for "no filter".
type listFilters struct {
	status       *string
	taskType     *string
	resourceType *string
	resourceID   *uuid.UUID
}

// parseListFilters extracts and validates the optional filter query
// params. Writes a 400 envelope on the first bad value and returns
// (zero, false). Caller stops on false.
func parseListFilters(w http.ResponseWriter, r *http.Request, q map[string][]string) (listFilters, bool) {
	var f listFilters

	if v := first(q, "status"); v != "" {
		if err := validation.ValidateTaskStatus(v); err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, err.Error(), nil)
			return f, false
		}
		s := v
		f.status = &s
	}
	if v := first(q, "type"); v != "" {
		t := v
		f.taskType = &t
	}
	if v := first(q, "resource_type"); v != "" {
		rt := v
		f.resourceType = &rt
	}
	if v := first(q, "resource_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, "resource_id must be a uuid", nil)
			return f, false
		}
		f.resourceID = &id
	}
	return f, true
}

// runList dispatches to ListTasksAny or ListTasksOwn based on the
// caller's RBAC scope for task:read. The middleware already gated the
// route on Has(role, task:read) — reaching ScopeNone here would be a
// matrix bug, but we still fall through safely with an empty result.
func (h *Handler) runList(
	ctx context.Context,
	caller *auth.User,
	f listFilters,
	cur *pagination.Cursor,
	limit int32,
) ([]store.Task, error) {
	switch auth.ScopeFor(caller.Role, auth.PermTaskRead) {
	case auth.ScopeAny:
		params := store.ListTasksAnyParams{
			LimitCount:         limit + 1,
			StatusFilter:       f.status,
			TypeFilter:         f.taskType,
			ResourceTypeFilter: f.resourceType,
			ResourceIDFilter:   f.resourceID,
		}
		if cur != nil {
			params.CursorCreatedAt = &cur.CreatedAt
			params.CursorID = &cur.ID
		}
		return h.store.ListTasksAny(ctx, params)

	case auth.ScopeOwn:
		id := caller.ID
		params := store.ListTasksOwnParams{
			CreatedBy:          &id,
			LimitCount:         limit + 1,
			StatusFilter:       f.status,
			TypeFilter:         f.taskType,
			ResourceTypeFilter: f.resourceType,
			ResourceIDFilter:   f.resourceID,
		}
		if cur != nil {
			params.CursorCreatedAt = &cur.CreatedAt
			params.CursorID = &cur.ID
		}
		return h.store.ListTasksOwn(ctx, params)

	default:
		// Defensive: middleware should have rejected the request before
		// reaching here. Empty result is the safe fall-through.
		return nil, nil
	}
}

// first returns the first value of key in the query map, or "" when
// absent. Mirrors the cursor-pagination convention used by the other list handlers.
func first(q map[string][]string, key string) string {
	if vs, ok := q[key]; ok && len(vs) > 0 {
		return vs[0]
	}
	return ""
}
