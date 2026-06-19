// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package blobs

import (
	"encoding/json"
	"net/http"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/response"
)

// StopServe handles POST /v1/blobs/stop-serve.
//
//   - Decodes the BlobStopServeRequest (the per-op token).
//   - Asks the serve manager to tear down the serve that token primed,
//     releasing its reserved port instead of waiting out the token TTL.
//   - Returns 204 (also when no live serve matches: the call is idempotent).
//
// Errors:
//   - 400 validation_failed - malformed body or a missing token.
func (h *Handler) StopServe(w http.ResponseWriter, r *http.Request) {
	var req agentapi.BlobStopServeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "request body is not valid JSON", nil)
		return
	}
	if req.Token == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "token is required", nil)
		return
	}

	h.stopper.StopServe(req.Token)
	response.WriteNoContent(w)
}
