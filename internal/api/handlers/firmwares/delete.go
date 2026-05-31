// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package firmwares

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// Delete implements DELETE /v1/firmwares/{id}. Required permission:
// firmware:manage (admin only). Refuses with 409 + blocking_resources
// when the firmware is still referenced by active vms or templates;
// firmwares have no force-delete counterpart by design (the operator
// must remove or migrate the dependent resources first).
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "id must be a uuid", nil)
		return
	}

	if err := h.store.DeleteFirmware(r.Context(), id); err != nil {
		writeDeleteError(w, r, err)
		return
	}

	response.WriteNoContent(w)
}

// writeDeleteError maps the error returned by store.DeleteFirmware to
// the standard envelope: ErrNotFound → 404, *ResourceInUseError → 409
// blocking_resources, anything else → 500.
func writeDeleteError(w http.ResponseWriter, r *http.Request, err error) {
	var inUse *store.ResourceInUseError
	switch {
	case errors.Is(err, store.ErrNotFound):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "firmware not found", nil)
	case errors.As(err, &inUse):
		response.WriteBlockingResources(w, r, &response.BlockingResourcesError{
			Message:   "firmware is in use; remove the dependent resources first",
			Resources: inUse.Resources,
		})
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete firmware", nil)
	}
}
