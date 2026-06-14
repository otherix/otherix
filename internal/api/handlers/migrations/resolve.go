// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// migrationIDPrefixMinLen is the shortest short-id prefix the resolver accepts.
// Eight hex characters is the git/docker convention and keeps accidental
// over-broad matches (which range the whole migration keyspace) out of the API.
const migrationIDPrefixMinLen = 8

// resolveMigration turns the {id} path param into a concrete migration. It
// accepts either a full UUID (resolved by MigrationByID, the today path) or a
// unique short hex prefix of the migration id (git/docker style): the prefix is
// range-scanned and resolves only when it matches exactly one migration.
//
// On any non-resolution it writes the response envelope itself and returns
// ok=false, so callers must return immediately. The branches:
//   - full UUID, found -> (migration, true)
//   - full UUID, absent -> 404 not_found
//   - prefix shorter than migrationIDPrefixMinLen or non-hex -> 400 validation_failed
//   - prefix, zero matches -> 404 not_found
//   - prefix, one match -> (migration, true)
//   - prefix, multiple matches -> 409 conflict (ambiguous)
//   - store error -> 500 internal
//
// It does NOT apply the ownership / visibility gate - that stays with the
// caller (Get / Cancel), which re-applies its existing check on the resolved
// migration's VM, so the no-existence-leak 404 rules are preserved.
func (h *Handler) resolveMigration(w http.ResponseWriter, r *http.Request) (store.Migration, bool) {
	raw := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "id")))

	if id, err := uuid.Parse(raw); err == nil && len(raw) == 36 {
		m, err := h.store.MigrationByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeMigrationNotFound(w, r)
				return store.Migration{}, false
			}
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "load migration", nil)
			return store.Migration{}, false
		}
		return m, true
	}

	if len(raw) < migrationIDPrefixMinLen || !isHex(raw) {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"migration id must be a uuid or a hex prefix of at least 8 chars", nil)
		return store.Migration{}, false
	}

	matches, err := h.store.MigrationsByIDPrefix(r.Context(), raw)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "resolve migration prefix", nil)
		return store.Migration{}, false
	}
	switch len(matches) {
	case 0:
		writeMigrationNotFound(w, r)
		return store.Migration{}, false
	case 1:
		return matches[0], true
	default:
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict, "ambiguous migration id prefix",
			map[string]any{"matches": len(matches)})
		return store.Migration{}, false
	}
}

// isHex reports whether s is non-empty and consists solely of lowercase hex
// digits. The resolver lowercases the raw id before calling this, so uppercase
// is normalised, not rejected. A UUID's dashes make it non-hex, which is why the
// full-UUID branch runs first.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
