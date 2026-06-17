// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package snapshots

import (
	"net/http"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
)

// writeVMResolveError maps a resolver.Error from the {id} path param to the
// VM 404 / UUID-rejection / 500 envelopes. VM identifiers are name-only - a
// UUID literal in the path is 400 validation_failed.
func writeVMResolveError(w http.ResponseWriter, r *http.Request, err error) {
	if resolver.IsUUIDInName(err) {
		response.WriteUUIDNotAllowedError(w, r, "vm", "id")
		return
	}
	if resolver.IsNotFound(err) {
		writeVMNotFound(w, r)
		return
	}
	response.WriteError(w, r, http.StatusInternalServerError,
		response.CodeInternal, "load vm", nil)
}

// writeVMNotFound writes the canonical VM 404. Cross-owner ownership denials map
// here too (no existence leak), so the envelope is shared by the "absent" and
// "invisible" cases.
func writeVMNotFound(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusNotFound,
		response.CodeVMNotFound, "vm not found", nil)
}

// writeSnapshotNotFound writes the canonical snapshot 404, shared by the
// "absent" and "invisible to caller" cases.
func writeSnapshotNotFound(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusNotFound,
		response.CodeNotFound, "snapshot not found", nil)
}

// derefString returns *s or "" when nil.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
