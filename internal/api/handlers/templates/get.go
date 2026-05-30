// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package templates

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// errTemplateInvisible is the in-flight signal that the row exists
// but the caller cannot see it. Surfaced as 404 (no existence leak).
var errTemplateInvisible = errors.New("template invisible to caller")

// Get implements GET /v1/templates/{id}. Required permission:
// `template:read:public` (universal-but-not-anonymous). Public
// templates are visible to every authenticated caller; private
// templates additionally require `template:read` at scope=any
// (admin/operator) or scope=own (developer; their own row only).
// Viewer cannot see any private template at all (matrix has no
// `template:read`).
//
// {id} is a template name; UUID literals are rejected with 400
// validation_failed at the resolver. Cross-user private access
// surfaces as 404, never 403, to prevent existence probing via
// differing status codes.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	row, err := h.resolveVisibleTemplate(r.Context(), chi.URLParam(r, "id"), caller)
	if err != nil {
		writeLoadError(w, r, err)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, toView(row))
}

// resolveVisibleTemplate folds the resolver lookup and the visibility /
// ownership check into a single helper. Returns:
//
//   - (row, nil) when the caller may see it.
//   - (zero, *resolver.Error{CodeNotFound}) when the row is missing
//     or soft-deleted.
//   - (zero, errTemplateInvisible) when the row exists but the
//     caller may not see it (private template owned by someone else,
//     and the caller does not hold `template:read` at scope=any).
//   - (zero, other) for unexpected DB errors.
func (h *Handler) resolveVisibleTemplate(ctx context.Context, identifier string, caller *auth.User) (store.Template, error) {
	row, err := resolver.Template(ctx, h.store, identifier)
	if err != nil {
		return store.Template{}, err
	}
	if row.Visibility == "public" {
		return row, nil
	}
	// Private — apply own-vs-any.
	if err := auth.CheckOwnership(caller, &row.OwnerID, auth.PermTemplateRead); err != nil {
		if errors.Is(err, auth.ErrPermissionDenied) {
			return store.Template{}, errTemplateInvisible
		}
		return store.Template{}, err
	}
	return row, nil
}

// writeLoadError maps an error from resolveVisibleTemplate (or any
// per-template lookup) to the canonical 404 / 400 / 500 envelopes.
// "row missing" and "exists but invisible" collapse to 404; UUID
// input in the path surfaces as 400 validation_failed.
func writeLoadError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case resolver.IsUUIDInName(err):
		response.WriteUUIDNotAllowedError(w, r, "template", "id")
	case resolver.IsNotFound(err),
		errors.Is(err, pgx.ErrNoRows),
		errors.Is(err, errTemplateInvisible):
		writeNotFound(w, r)
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load template", nil)
	}
}
