// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// Delete implements DELETE /v1/storage-pools/{id}. Required
// permission: storage_pool:manage (admin only). {id} accepts either a
// UUID literal or a pool name. Refuses with 409 + blocking_resources
// when the pool still hosts active vm_disks references; storage pools
// have no force-delete counterpart by design (the operator must
// remove or migrate the dependent disks first).
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	row, err := resolver.Pool(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writePoolResolveError(w, r, err, "storage pool not found", "load storage pool")
		return
	}

	if err := h.store.DeleteStoragePool(r.Context(), row.ID); err != nil {
		writeDeleteError(w, r, err)
		return
	}

	response.WriteNoContent(w)
}

// writeDeleteError maps the store domain error to the standard
// envelope. A *store.ResourceInUseError carries the blocking counts;
// a storage pool is now only ever blocked by attached vm_disks (the
// image cache is agent-owned and not a CP-side delete blocker), so the
// 409 keeps the generic `conflict` code and the vm_disks count.
func writeDeleteError(w http.ResponseWriter, r *http.Request, err error) {
	var blocking *store.ResourceInUseError
	if !errors.As(err, &blocking) {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete storage pool", nil)
		return
	}
	response.WriteBlockingResources(w, r, &response.BlockingResourcesError{
		Message:   "storage pool is in use by virtual machine disks; remove or migrate them first",
		Resources: map[string]int64{"vm_disks": blocking.Resources["vm_disks"]},
	})
}
