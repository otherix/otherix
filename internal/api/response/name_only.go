// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package response

import "net/http"

// WriteUUIDNotAllowedError emits the standard 400 validation_failed
// envelope for paths/bodies/filters that accept resource names only.
// The structured `details` payload carries (field, reason, hint) so
// CLI scripts can branch on the rejection reason without parsing the
// human-readable message.
//
// resource — operator-facing kind (e.g. "vm", "template", "node");
// field — wire location of the offending value (path param name,
// body field name, query param name); kept identical to the user's
// input so error renderers can echo back.
func WriteUUIDNotAllowedError(w http.ResponseWriter, r *http.Request, resource, field string) {
	WriteError(w, r, http.StatusBadRequest,
		CodeValidationFailed,
		"uuid format not accepted for name-only parameter '"+field+"'",
		map[string]any{
			"field":  field,
			"reason": "uuid_not_accepted_in_name_path",
			"hint":   "Use 'otherix " + resource + " list' to discover names.",
		})
}
