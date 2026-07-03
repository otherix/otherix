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

// GatewayEnable implements POST /v1/nodes/{id}/gateway/enable. It assigns the
// ingress-gateway role to a node. Idempotent: a node that already holds the
// role returns 200 with its current view. Required permission: node:manage
// (admin). {id} is name-only; UUID literals are rejected with 400
// validation_failed at the resolver.
func (h *Handler) GatewayEnable(w http.ResponseWriter, r *http.Request) {
	h.setGatewayRole(w, r, true)
}

// GatewayDisable implements POST /v1/nodes/{id}/gateway/disable. It removes the
// ingress-gateway role from a node. Idempotent: a node that does not hold the
// role returns 200 with its current view. Required permission: node:manage
// (admin).
func (h *Handler) GatewayDisable(w http.ResponseWriter, r *http.Request) {
	h.setGatewayRole(w, r, false)
}

// setGatewayRole resolves the node named by {id} and writes its gateway role to
// enabled via the store's compare-and-set. A concurrent write surfaces as a
// retryable 409 rather than a fault. Missing (or caller-invisible) nodes are
// 404 not_found so node existence never leaks.
func (h *Handler) setGatewayRole(w http.ResponseWriter, r *http.Request, enabled bool) {
	current, err := resolver.Node(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writeNodeResolveError(w, r, err)
		return
	}

	updated, err := h.store.SetNodeGatewayRole(r.Context(), current.ID, enabled)
	if err != nil {
		if errors.Is(err, store.ErrConcurrentUpdate) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "node was modified concurrently, retry", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "set node gateway role", nil)
		return
	}

	ownsPool, err := h.nodeOwnsPool(r.Context(), updated.ID)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load node pools", nil)
		return
	}
	writeNodeResponse(w, r, http.StatusOK, updated, ownsPool, response.WriteJSON)
}
