// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
)

// Get implements GET /v1/nodes/{id}. Required permission: node:read.
// admin / operator receive the full nodeView (including cpu_cores_
// effective / memory_effective_mib); developer / viewer
// receive the reduced nodeSummaryView. {id} is name-only; UUID
// literals are rejected with 400 validation_failed at the resolver.
//
// Two queries: resolver.Node validates UUID-rejection и returns the
// resolved row's id (по name lookup); GetNodeEffectiveByID re-fetches
// from the node_effective_availability view к surface effective
// columns. The double round-trip is acceptable at MVP scale; а
// future iteration could resolve by name directly on the view if
// the cost matters.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	resolved, err := resolver.Node(r.Context(), h.store.Queries(), chi.URLParam(r, "id"))
	if err != nil {
		writeNodeResolveError(w, r, err)
		return
	}

	row, err := h.store.Queries().GetNodeEffectiveByID(r.Context(), resolved.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Soft-deleted between resolver и view lookup — surface as
			// 404 same as resolver's not-found path.
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "node not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load node", nil)
		return
	}

	writeNodeResponseEffective(w, r, http.StatusOK, row, response.WriteJSON)
}
