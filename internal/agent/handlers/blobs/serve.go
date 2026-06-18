// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package blobs

import (
	"encoding/json"
	"net/http"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/response"
)

// Serve handles POST /v1/blobs/serve.
//
//   - Decodes the BlobServeRequest (digest + token + consumer_node_id).
//   - Asks the serve manager to open a peer-facing listener for the blob.
//   - Returns 200 with the reachable serve_endpoint + expires_at.
//
// Errors:
//   - 400 validation_failed - malformed body or a missing required field.
//   - 500 internal - the serve manager could not open a listener.
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request) {
	var req agentapi.BlobServeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "request body is not valid JSON", nil)
		return
	}
	if req.Digest == "" || req.Token == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "digest and token are required", nil)
		return
	}

	endpoint, expiresAt, err := h.server.Serve(req.Digest, req.Token, req.ConsumerNodeID.String())
	if err != nil {
		h.log.ErrorContext(r.Context(), "blob serve failed",
			"digest", req.Digest, "err", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal error", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, map[string]any{
		"serve_endpoint": endpoint,
		"expires_at":     expiresAt,
	})
}
