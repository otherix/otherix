// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package templates

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/pagination"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// List implements GET /v1/templates. Required permission:
// `template:read:public` — every authenticated role holds it. The
// visibility/ownership clamp lives in the SQL parameter pair
// `caller_can_see_any` + `caller_id`:
//
//   - admin / operator → caller_can_see_any=true (caller_id ignored).
//     Every active template visible.
//   - developer → caller_can_see_any=false, caller_id=caller.ID.
//     Public templates plus the developer's own private set.
//   - viewer → caller_can_see_any=false, caller_id=NULL. Public only.
//
// Optional query filters (?owner_id, ?visibility, ?architecture,
// ?os_family) compose with the clamp via AND, so a non-admin caller
// who supplies ?visibility=private sees only their own private
// templates (developer) or an empty list (viewer); and a non-admin
// caller who supplies ?owner_id targeting another user sees only that
// user's public templates.
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
		writeValidation(w, r, "invalid cursor")
		return
	}

	params, ok := buildListParams(w, r, caller, q, limit)
	if !ok {
		return
	}
	if cur != nil {
		params.CursorCreatedAt = &cur.CreatedAt
		params.CursorID = &cur.ID
	}

	rows, err := h.store.ListTemplates(r.Context(), params)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list templates", nil)
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

	views := make([]templateView, 0, len(rows))
	for _, t := range rows {
		views = append(views, toView(t))
	}
	response.WriteJSON(w, r, http.StatusOK, listResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	})
}

// buildListParams reads the query string, applies the per-role clamp,
// and assembles the SQL parameters. Returns (params, false) after
// writing a 400 response when any filter is malformed.
func buildListParams(w http.ResponseWriter, r *http.Request, caller *auth.User, q map[string][]string, limit int32) (store.ListTemplatesParams, bool) {
	params := store.ListTemplatesParams{LimitCount: limit + 1}
	applyRoleClamp(caller, &params)

	if v := first(q, "owner_id"); v != "" {
		ownerID, err := uuid.Parse(v)
		if err != nil {
			writeValidation(w, r, "owner_id must be a uuid")
			return params, false
		}
		params.OwnerIDFilter = &ownerID
	}
	if v := first(q, "visibility"); v != "" {
		if err := validation.ValidateVisibility(v); err != nil {
			writeValidation(w, r, err.Error())
			return params, false
		}
		vis := v
		params.VisibilityFilter = &vis
	}
	if v := first(q, "architecture"); v != "" {
		if err := validation.ValidateArchitecture(v); err != nil {
			writeValidation(w, r, err.Error())
			return params, false
		}
		arch := store.CPUArch(v)
		params.ArchitectureFilter = &arch
	}
	if v := first(q, "os_family"); v != "" {
		if err := validation.ValidateOSFamily(v); err != nil {
			writeValidation(w, r, err.Error())
			return params, false
		}
		fam := store.OsFamily(v)
		params.OsFamilyFilter = &fam
	}
	return params, true
}

// applyRoleClamp sets caller_can_see_any and caller_id on params per
// the caller's role. Admin / operator see everything; developer sees
// public + own; viewer sees public only.
func applyRoleClamp(caller *auth.User, params *store.ListTemplatesParams) {
	switch auth.ScopeFor(caller.Role, auth.PermTemplateRead) {
	case auth.ScopeAny:
		params.CallerCanSeeAny = true
	case auth.ScopeOwn:
		id := caller.ID
		params.CallerID = &id
	default:
		// No template:read at all (viewer). Public-only — fall through
		// with caller_can_see_any=false, caller_id=nil.
	}
}

// first returns the first value of key in the query map, or "" when
// absent. http.Request.URL.Query() returns the same shape.
func first(q map[string][]string, key string) string {
	if vs, ok := q[key]; ok && len(vs) > 0 {
		return vs[0]
	}
	return ""
}
