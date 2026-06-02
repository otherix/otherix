// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Get implements GET /v1/nodes/{id}. Required permission: node:read.
// admin / operator receive the full nodeView (including cpu_cores_
// effective / memory_effective_mib); developer / viewer
// receive the reduced nodeSummaryView. {id} is name-only; UUID
// literals are rejected with 400 validation_failed at the resolver.
//
// Two queries: resolver.Node validates UUID-rejection and returns the
// resolved row's id (by name lookup); GetNodeEffectiveByID re-fetches
// from the node_effective_availability view to surface effective
// columns. The double round-trip is acceptable at MVP scale; a
// future iteration could resolve by name directly on the view if
// the cost matters.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	resolved, err := resolver.Node(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writeNodeResolveError(w, r, err)
		return
	}

	row, err := h.store.NodeEffectiveByID(r.Context(), resolved.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Soft-deleted between resolver and view lookup — surface as
			// 404 same as resolver's not-found path.
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "node not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load node", nil)
		return
	}

	// network_conditions is the per-(node, network) materialisation list,
	// an admin / operator-only detail. Resolve it only for those roles so
	// developer / viewer reads do not pay the extra fan-out (and the
	// summary view drops it regardless). NetworkByID skips stale rows
	// whose network was deleted, so a lingering status never 500s the read.
	var conditions []networkConditionView
	var wg *wireguardView
	if user := auth.UserFromContext(r.Context()); user != nil &&
		(user.Role == auth.RoleAdmin || user.Role == auth.RoleOperator) {
		conditions, err = networkConditions(r.Context(), h.store, resolved.ID)
		if err != nil {
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "load node network conditions", nil)
			return
		}
		wg, err = nodeWireguard(r.Context(), h.store, resolved.ID)
		if err != nil {
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "load node wireguard fabric", nil)
			return
		}
	}

	writeNodeResponseEffective(w, r, http.StatusOK, row, conditions, wg, response.WriteJSON)
}
