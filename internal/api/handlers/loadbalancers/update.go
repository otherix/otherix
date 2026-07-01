// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Update implements PATCH /v1/loadbalancers/{id}, where {id} is the
// load-balancer NAME. Required permission: loadbalancer:update. After the row
// loads, auth.CheckOwnership runs; a cross-owner attempt surfaces as 404 (never
// 403), mirroring the VM broker so existence is not leaked.
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "authentication required", nil)
		return
	}

	name := chi.URLParam(r, "id")

	var req updateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}

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

	if err := auth.CheckOwnership(user, &row.OwnerID, auth.PermLoadBalancerUpdate); err != nil {
		// Cross-owner invisibility: a caller who does not own the row must not
		// learn it exists, so this is 404, not 403.
		h.writeNotFound(w, r)
		return
	}

	if req.Name != nil {
		newName := strings.TrimSpace(*req.Name)
		if err := validateName(newName); err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, err.Error(), nil)
			return
		}
		row.Name = newName
	}
	if req.Port != nil {
		if err := validatePort(*req.Port); err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, err.Error(), nil)
			return
		}
		row.Port = *req.Port
	}
	if req.Selector != nil {
		if err := validateSelector(*req.Selector); err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, err.Error(), nil)
			return
		}
		row.Selector = *req.Selector
	}

	updated, err := h.store.UpdateLoadBalancer(r.Context(), store.UpdateLoadBalancerParams{
		ID:       row.ID,
		Name:     row.Name,
		Port:     row.Port,
		Selector: row.Selector,
	})
	if err != nil {
		if errors.Is(err, store.ErrLoadBalancerNameExists) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "load balancer name already in use", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "update load balancer", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, toView(updated))
}

// writeNotFound emits the standard load-balancer 404 with the dedicated
// loadbalancer_not_found code.
func (h *Handler) writeNotFound(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusNotFound,
		response.CodeLoadBalancerNotFound, "load balancer not found", nil)
}
