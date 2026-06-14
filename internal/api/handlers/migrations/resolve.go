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
//   - prefix shorter than migrationIDPrefixMinLen or not a uuid-string prefix -> 400 validation_failed
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

	if len(raw) < migrationIDPrefixMinLen || !isUUIDPrefix(raw) {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"migration id must be a uuid or a unique id prefix of at least 8 chars", nil)
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

// isUUIDPrefix reports whether s is a non-empty prefix of a canonical UUID
// string (lowercase hex with hyphens at positions 8, 13, 18, 23). The short id
// the CLI shows is the first N characters of the UUID *string*, so for N > 8 it
// includes a hyphen - a pure-hex check would reject those round-tripped ids.
// The resolver lowercases s first, so uppercase is normalised, not rejected.
func isUUIDPrefix(s string) bool {
	if s == "" || len(s) > 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
