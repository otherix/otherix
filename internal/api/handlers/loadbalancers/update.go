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
	"github.com/otherix/otherix/internal/api/validation"
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

	if !applyUpdateBaseFields(w, r, &req, &row) {
		return
	}

	if !applyUpdatePublish(w, r, user, &req, &row) {
		return
	}

	updated, err := h.store.UpdateLoadBalancer(r.Context(), store.UpdateLoadBalancerParams{
		ID:            row.ID,
		Name:          row.Name,
		Port:          row.Port,
		Selector:      row.Selector,
		HealthCheck:   row.HealthCheck,
		PublishedPort: row.PublishedPort,
		Protocol:      row.Protocol,
		SourceCIDRs:   row.SourceCIDRs,
	})
	if err != nil {
		if errors.Is(err, store.ErrLoadBalancerNameExists) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "load balancer name already in use", nil)
			return
		}
		if errors.Is(err, store.ErrLoadBalancerPublishedPortExists) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "published port already in use", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "update load balancer", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, toView(updated))
}

// applyUpdateBaseFields applies the non-published tri-state fields (name, port,
// selector, health check) from req onto row, validating each present field. It
// writes the error response and returns false when the request must be rejected.
func applyUpdateBaseFields(w http.ResponseWriter, r *http.Request, req *updateRequest, row *store.LoadBalancer) bool {
	if req.Name != nil {
		newName := strings.TrimSpace(*req.Name)
		if err := validateName(newName); err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, err.Error(), nil)
			return false
		}
		row.Name = newName
	}
	if req.Port != nil {
		if err := validatePort(*req.Port); err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, err.Error(), nil)
			return false
		}
		row.Port = *req.Port
	}
	if req.Selector != nil {
		if err := validateSelector(*req.Selector); err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, err.Error(), nil)
			return false
		}
		row.Selector = *req.Selector
	}
	hc, err := resolveHealthCheck(req.HealthCheck, normalizeUpdateBase(row.HealthCheck))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return false
	}
	row.HealthCheck = hc
	return true
}

// applyUpdatePublish enforces the loadbalancer:publish gate over the whole
// public-exposure surface and applies the tri-state publish fields onto row. Any
// mutation to published_port, protocol, OR source_cidrs requires the publish
// capability - not published_port alone: an LB may be owned by a lower-privileged
// user (a developer owning a listener an operator published), and without
// covering protocol and source_cidrs that owner could PATCH {"source_cidrs":[]}
// to strip the allowlist with only loadbalancer:update. It writes the error
// response and returns false when the request must be rejected.
func applyUpdatePublish(w http.ResponseWriter, r *http.Request, user *auth.User, req *updateRequest, row *store.LoadBalancer) bool {
	touchesPublish := req.PublishedPort != nil || req.Protocol != nil || req.SourceCIDRs != nil
	if touchesPublish && !auth.Has(user.Role, auth.PermLoadBalancerPublish) {
		response.WriteError(w, r, http.StatusForbidden,
			response.CodePermissionDenied,
			"loadbalancer:publish required to change a published listener", nil)
		return false
	}
	if req.PublishedPort != nil {
		if *req.PublishedPort == 0 {
			// 0 is the unpublish sentinel: clear the port, protocol, and allowlist
			// together so no orphan exposure state survives.
			row.PublishedPort = nil
			row.Protocol = ""
			row.SourceCIDRs = nil
		} else {
			if err := validatePort(*req.PublishedPort); err != nil {
				response.WriteError(w, r, http.StatusBadRequest,
					response.CodeValidationFailed, err.Error(), nil)
				return false
			}
			p := *req.PublishedPort
			row.PublishedPort = &p
			if row.Protocol == "" {
				row.Protocol = validation.DefaultLBProtocol
			}
		}
	}
	if req.Protocol != nil {
		if err := validation.ValidateLBProtocol(*req.Protocol); err != nil {
			response.WriteError(w, r, http.StatusBadRequest,
				response.CodeValidationFailed, err.Error(), nil)
			return false
		}
		row.Protocol = *req.Protocol
	}
	if req.SourceCIDRs != nil {
		for _, c := range *req.SourceCIDRs {
			if err := validation.ValidateSourceCIDR(c); err != nil {
				response.WriteError(w, r, http.StatusBadRequest,
					response.CodeValidationFailed, err.Error(), nil)
				return false
			}
		}
		row.SourceCIDRs = *req.SourceCIDRs
	}
	return true
}

// writeNotFound emits the standard load-balancer 404 with the dedicated
// loadbalancer_not_found code.
func (h *Handler) writeNotFound(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusNotFound,
		response.CodeLoadBalancerNotFound, "load balancer not found", nil)
}
