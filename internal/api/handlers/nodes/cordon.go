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

// Cordon implements POST /v1/nodes/{id}/cordon. Required permission:
// node:maintenance (admin / operator). Marks the node as cordoned so
// the scheduler stops placing new VMs onto it. Idempotent: cordoning an
// already-cordoned node is a no-op (200 with the current row). 409 for
// non-cordonable states (gone, draining). {id} is name-only; UUID
// literals are rejected with 400 validation_failed at the resolver.
func (h *Handler) Cordon(w http.ResponseWriter, r *http.Request) {
	current, err := resolver.Node(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writeNodeResolveError(w, r, err)
		return
	}

	switch current.Status {
	case store.NodeStatusCordoned:
		writeNodeResponse(w, r, http.StatusOK, current, response.WriteJSON)
		return
	case store.NodeStatusGone, store.NodeStatusDraining:
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict,
			"node cannot be cordoned in its current state",
			map[string]any{"current_status": string(current.Status)})
		return
	}

	updated, err := h.store.Queries().CordonNode(r.Context(), current.ID)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "cordon node", nil)
		return
	}
	writeNodeResponse(w, r, http.StatusOK, updated, response.WriteJSON)
}
