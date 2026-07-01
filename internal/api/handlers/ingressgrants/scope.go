// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ingressgrants

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

// AddVM implements POST /v1/ingress-grants/{id}/vms. Required permission:
// vm:ingress-grant. The caller must own the grant (else 404) and, when
// owner-scoped, the VM being added (else 403). Adding an existing vm_name
// replaces its login. Returns 200 with the updated grant.
func (h *Handler) AddVM(w http.ResponseWriter, r *http.Request) {
	grant, ok := h.loadOwnedGrant(w, r)
	if !ok {
		return
	}
	caller := auth.UserFromContext(r.Context())

	var body createVM
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return
	}
	entry, err := validateVM(body)
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, err.Error(), nil)
		return
	}

	if !h.authorizeVMs(w, r, caller, []store.IngressGrantVM{entry}) {
		return
	}

	updated, err := h.store.AddIngressGrantVM(r.Context(), grant.ID, entry)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeGrantNotFound(w, r)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "add vm to ingress grant", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, toView(updated))
}

// RemoveVM implements DELETE /v1/ingress-grants/{id}/vms/{vm_name}. Required
// permission: vm:ingress-grant. The caller must own the grant (else 404).
// Removing a vm_name not in the grant is a no-op. Shrinking one's own
// grant needs no per-VM ownership check. Returns 200 with the updated
// grant.
func (h *Handler) RemoveVM(w http.ResponseWriter, r *http.Request) {
	grant, ok := h.loadOwnedGrant(w, r)
	if !ok {
		return
	}

	vmName := strings.TrimSpace(chi.URLParam(r, "vm_name"))
	if vmName == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "vm_name is required", nil)
		return
	}

	updated, err := h.store.RemoveIngressGrantVM(r.Context(), grant.ID, vmName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeGrantNotFound(w, r)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "remove vm from ingress grant", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, toView(updated))
}
