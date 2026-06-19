// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package blobs

import (
	"encoding/json"
	"net/http"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/response"
)

// Pull handles POST /v1/blobs/pull.
//
//   - Decodes the BlobPullRequest (digest + token + holder_endpoint).
//   - Asks the puller to start an agent task fetching the blob into the local
//     artifact store.
//   - Returns 202 with the task_id immediately; the CP polls
//     /v1/tasks/{task_id} to observe terminal status.
//
// Errors:
//   - 400 validation_failed - malformed body or a missing required field.
//   - 500 internal - the puller could not start the task.
func (h *Handler) Pull(w http.ResponseWriter, r *http.Request) {
	var req agentapi.BlobPullRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "request body is not valid JSON", nil)
		return
	}
	if req.Digest == "" || req.Token == "" || req.HolderEndpoint == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "digest, token and holder_endpoint are required", nil)
		return
	}

	var holderIdentity string
	if req.HolderIdentity != nil {
		holderIdentity = *req.HolderIdentity
	}
	var expectedSize int64
	if req.ExpectedSize != nil {
		expectedSize = *req.ExpectedSize
	}
	taskID, err := h.puller.Pull(req.Digest, req.Token, req.HolderEndpoint, holderIdentity, expectedSize)
	if err != nil {
		h.log.ErrorContext(r.Context(), "blob pull failed",
			"digest", req.Digest, "holder_endpoint", req.HolderEndpoint, "err", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "internal error", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusAccepted, asyncAccepted{
		TaskID: taskID,
		Status: "pending",
		Links:  map[string]any{"self": "/v1/tasks/" + taskID},
	})
}
