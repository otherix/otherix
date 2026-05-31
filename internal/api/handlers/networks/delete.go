// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package networks

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// Delete implements DELETE /v1/networks/{id}. Required permission:
// network:manage (admin only). Refuses with 409 + blocking_resources
// when the network still has active vm_nics references; networks
// have no force-delete counterpart by design (the operator must
// remove or migrate the dependent VMs first).
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "id must be a uuid", nil)
		return
	}

	if err := h.store.DeleteNetwork(r.Context(), id); err != nil {
		writeDeleteError(w, r, err)
		return
	}

	response.WriteNoContent(w)
}

// writeDeleteError maps the error returned by store.DeleteNetwork to
// the standard envelope: ErrNotFound → 404, *ResourceInUseError → 409
// blocking_resources, anything else → 500.
func writeDeleteError(w http.ResponseWriter, r *http.Request, err error) {
	var inUse *store.ResourceInUseError
	switch {
	case errors.Is(err, store.ErrNotFound):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "network not found", nil)
	case errors.As(err, &inUse):
		response.WriteBlockingResources(w, r, &response.BlockingResourcesError{
			Message:   "network is in use by virtual machine NICs; remove or migrate them first",
			Resources: inUse.Resources,
		})
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete network", nil)
	}
}
