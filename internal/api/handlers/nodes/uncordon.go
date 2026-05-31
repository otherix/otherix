// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// Uncordon implements POST /v1/nodes/{id}/uncordon. Required permission:
// node:maintenance (admin / operator). Returns the node to `ready` so
// the scheduler may target it again. Idempotent: uncordoning a `ready`
// node is a no-op. 409 for states from which uncordon makes no sense
// (pending, unreachable, draining, gone). {id} is name-only; UUID
// literals are rejected with 400 validation_failed at the resolver.
func (h *Handler) Uncordon(w http.ResponseWriter, r *http.Request) {
	current, err := resolver.Node(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writeNodeResolveError(w, r, err)
		return
	}

	switch current.Status {
	case store.NodeStatusReady:
		writeNodeResponse(w, r, http.StatusOK, current, response.WriteJSON)
		return
	case store.NodeStatusPending,
		store.NodeStatusDraining,
		store.NodeStatusUnreachable,
		store.NodeStatusGone:
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict,
			"node cannot be uncordoned in its current state",
			map[string]any{"current_status": string(current.Status)})
		return
	}

	updated, err := h.store.UncordonNode(r.Context(), current.ID)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "uncordon node", nil)
		return
	}
	writeNodeResponse(w, r, http.StatusOK, updated, response.WriteJSON)
}
