// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package templates

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// allowedCloneKeys is the exhaustive set of keys POST
// /v1/templates/{id}/clone accepts in its request body. Anything
// outside this set lands in `details.forbidden_fields` — clone is a
// metadata copy, not a remix; to override sizing or capabilities
// after clone the caller PATCHes the new id.
var allowedCloneKeys = []string{"name", "description"}

// Clone implements POST /v1/templates/{id}/clone. Required
// permission: `template:create` (the operation produces a new
// template, owned by the caller). {id} is a template name; UUID
// literals are rejected with 400 validation_failed at the resolver.
// Source visibility is enforced via the same own/any rule as Get:
// public sources are always cloneable (every role with
// `template:create` also holds `template:read:public`); private
// sources require either ownership or `template:read` at scope=any.
// Cross-user private surfaces as 404, never 403.
//
// The new row is owned by the caller, named per the request, and
// always starts with `visibility = 'private'` (Variant C). Image-source
// bundle (url + checksum + format + size), sizing, capabilities,
// metadata, firmware pin, os_variant and architecture / os_family are
// copied verbatim from the source.
func (h *Handler) Clone(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if extra := scanAllowedKeys(body, allowedCloneKeys); len(extra) > 0 {
		writeForbiddenFields(w, r, extra)
		return
	}

	var req cloneRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeValidation(w, r, "invalid request body")
		return
	}
	if err := validation.ValidateTemplateName(req.Name); err != nil {
		writeValidation(w, r, err.Error())
		return
	}

	source, err := h.resolveVisibleTemplate(r.Context(), chi.URLParam(r, "id"), caller)
	if err != nil {
		writeLoadError(w, r, err)
		return
	}

	desc, err := decodeNullableString(req.Description)
	if err != nil {
		writeValidation(w, r, "description must be null or a string")
		return
	}

	row, err := h.store.CloneTemplate(r.Context(), store.CloneTemplateParams{
		NewID:          uuid.New(),
		NewOwnerID:     caller.ID,
		NewName:        req.Name,
		NewDescription: desc,
		SourceID:       source.ID,
	})
	if err != nil {
		writeCloneError(w, r, err)
		return
	}

	response.WriteJSON(w, r, http.StatusCreated, toView(row))
}

// writeCloneError maps the store domain error to the standard
// envelope. Sources that have been deleted between resolveVisibleTemplate
// and CloneTemplate yield store.ErrNotFound from the store layer - the
// race is rare but preserved as 404. Name uniqueness collisions surface
// with the same neutral message used by Create.
func writeCloneError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeNotFound(w, r)
	case errors.Is(err, store.ErrTemplateNameExists):
		writeNeutralNameConflict(w, r)
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "clone template", nil)
	}
}
