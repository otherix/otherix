// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package templates

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// SetVisibility implements POST /v1/templates/{id}/set-visibility.
// Required permission: `template:set_visibility` (admin/operator:
// any). Ownership is intentionally NOT checked — admin/operator
// moderate the public catalogue without owning the rows; developer
// and viewer simply lack the permission and the route's middleware
// short-circuits with 403 before this handler runs. {id} is a
// template name; UUID literals are rejected with 400
// validation_failed at the resolver.
//
// Same-value short-circuit: when the requested visibility equals the
// current visibility we return the existing row 200 without issuing
// the UPDATE. This keeps `updated_at` a meaningful change marker —
// the column has an `updated_at` trigger that would otherwise advance
// on a no-op flip.
//
// Owner is preserved across visibility changes - publishing a private
// template to public does NOT transfer authorship to admin/operator.
// See docs/rbac.md.
func (h *Handler) SetVisibility(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req setVisibilityRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeValidation(w, r, "invalid request body")
		return
	}
	if err := validation.ValidateVisibility(req.Visibility); err != nil {
		writeValidation(w, r, err.Error())
		return
	}

	row, err := resolver.Template(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writeLoadError(w, r, err)
		return
	}

	// Same-value path: skip the UPDATE so updated_at remains a
	// meaningful change marker. Return the loaded row verbatim.
	if row.Visibility == req.Visibility {
		response.WriteJSON(w, r, http.StatusOK, toView(row))
		return
	}

	updated, err := h.store.Queries().SetTemplateVisibility(r.Context(),
		store.SetTemplateVisibilityParams{
			ID:         row.ID,
			Visibility: req.Visibility,
		})
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "update template visibility", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, toView(updated))
}
