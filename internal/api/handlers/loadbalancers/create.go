// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Create implements POST /v1/loadbalancers. Required permission:
// loadbalancer:create. The owner is stamped from the authenticated caller (not
// the request body). Returns 201 with the projected load balancer on success,
// 409 conflict on a name collision.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "authentication required", nil)
		return
	}

	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if err := validateName(req.Name); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}
	if err := validatePort(req.Port); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}
	if err := validateSelector(req.Selector); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}

	row, err := h.store.CreateLoadBalancer(r.Context(), store.CreateLoadBalancerParams{
		ID:       uuid.New(),
		Name:     req.Name,
		OwnerID:  user.ID,
		Port:     req.Port,
		Selector: req.Selector,
	})
	if err != nil {
		if errors.Is(err, store.ErrLoadBalancerNameExists) {
			response.WriteError(w, r, http.StatusConflict,
				response.CodeConflict, "load balancer name already in use", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "persist load balancer", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusCreated, toView(row))
}

// validateName checks the load-balancer name (trimmed by the caller): non-empty
// and free of '/' (which would poison the etcd name-guard key path).
func validateName(name string) error {
	switch {
	case name == "":
		return errors.New("name is required")
	case strings.ContainsRune(name, '/'):
		return errors.New("name must not contain '/'")
	}
	return nil
}

// validatePort enforces the TCP port range 1..65535.
func validatePort(port int32) error {
	if port < 1 || port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}

// validateSelector enforces the backend-selector invariants shared by create
// and update: the map must be non-empty, and every key AND value must be
// non-empty. An empty key or an empty value would match VMs that merely lack the
// label, silently widening the backend set.
func validateSelector(sel map[string]string) error {
	if len(sel) == 0 {
		return errors.New("selector must not be empty")
	}
	for k, v := range sel {
		if k == "" {
			return errors.New("selector keys must not be empty")
		}
		if v == "" {
			return errors.New("selector values must not be empty")
		}
	}
	return nil
}
