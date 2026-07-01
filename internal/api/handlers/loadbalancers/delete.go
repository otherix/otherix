// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Delete implements DELETE /v1/loadbalancers/{id}, where {id} is the
// load-balancer NAME. Required permission: loadbalancer:delete. After the row
// loads, auth.CheckOwnership runs; a cross-owner attempt surfaces as 404. The
// delete is unconditional (soft-delete) - a load balancer has no dependent
// resources to block on.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "authentication required", nil)
		return
	}

	name := chi.URLParam(r, "id")

	row, err := h.store.LoadBalancerByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeNotFound(w, r)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load load balancer", nil)
		return
	}

	if err := auth.CheckOwnership(user, &row.OwnerID, auth.PermLoadBalancerDelete); err != nil {
		h.writeNotFound(w, r)
		return
	}

	if err := h.store.DeleteLoadBalancer(r.Context(), row.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeNotFound(w, r)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete load balancer", nil)
		return
	}

	response.WriteNoContent(w)
}
