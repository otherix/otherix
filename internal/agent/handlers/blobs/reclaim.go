// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package blobs

import (
	"encoding/json"
	"net/http"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/response"
)

// Reclaim handles POST /v1/blobs/reclaim.
//
//   - Decodes the BlobReclaimRequest (the blob digest).
//   - Validates the digest is 64 lowercase hex chars.
//   - Asks the local artifact store to delete the blob and its sidecar.
//   - Returns 204 (also when the blob is absent: the call is idempotent).
//
// The Control Plane is the sole authority on whether a blob is still needed;
// this handler deletes exactly the named digest.
//
// Errors:
//   - 400 validation_failed - malformed body or a non-hex digest.
func (h *Handler) Reclaim(w http.ResponseWriter, r *http.Request) {
	var req agentapi.BlobReclaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "request body is not valid JSON", nil)
		return
	}
	if !isHexDigest(req.Digest) {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "digest must be 64 lowercase hex characters", nil)
		return
	}
	if err := h.deleter.Delete(req.Digest); err != nil {
		h.log.Error("blob reclaim delete failed", "digest", req.Digest, "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete blob failed", nil)
		return
	}
	response.WriteNoContent(w)
}

// isHexDigest reports whether s is exactly 64 lowercase hex characters.
func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
