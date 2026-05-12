// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package response

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// WriteJSON writes body as JSON with the given status. A nil body produces
// an empty response with the status only — useful for 204-equivalents that
// keep an explicit status (use WriteNoContent for the canonical 204).
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Default().ErrorContext(r.Context(), "encoding response failed", "error", err)
	}
}

// WriteNoContent writes a 204 with no body.
func WriteNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}
