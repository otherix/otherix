// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// Readmit implements POST /v1/nodes/{id}/readmit. Required permission:
// node:maintenance (admin / operator). Returns a node stuck in the terminal
// `gone` status to `pending` so the cluster re-accepts its heartbeats; the
// existing promotion path advances it to `ready` on the next fresh heartbeat.
// Idempotent: readmitting a `pending` node is a no-op. 409 for states from
// which readmit makes no sense (ready, cordoned, draining, unreachable). {id}
// is name-only; UUID literals are rejected with 400 validation_failed at the
// resolver.
func (h *Handler) Readmit(w http.ResponseWriter, r *http.Request) {
	current, err := resolver.Node(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writeNodeResolveError(w, r, err)
		return
	}

	switch current.Status {
	case store.NodeStatusPending:
		writeNodeResponse(w, r, http.StatusOK, current, response.WriteJSON)
		return
	case store.NodeStatusReady,
		store.NodeStatusCordoned,
		store.NodeStatusDraining,
		store.NodeStatusUnreachable:
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict,
			"node cannot be readmitted in its current state",
			map[string]any{"current_status": string(current.Status)})
		return
	}

	updated, err := h.store.ReadmitNode(r.Context(), current.ID)
	if err != nil {
		if errors.Is(err, store.ErrConcurrentUpdate) {
			// A liveness sweep (or other writer) flipped the node out from under
			// the handler's status check. Surface a retryable conflict, not a fault.
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "node was modified concurrently, retry", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "readmit node", nil)
		return
	}
	writeNodeResponse(w, r, http.StatusOK, updated, response.WriteJSON)
}
