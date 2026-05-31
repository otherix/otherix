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
// the handler owns the wire policy of how those counts become a 409.
// The storage-image branch carries the endpoint-specific
// resource_in_use code; the vm_disks-only branch keeps the generic
// `conflict` to preserve the historical wire contract.
func writeDeleteError(w http.ResponseWriter, r *http.Request, err error) {
	var blocking *store.ResourceInUseError
	if !errors.As(err, &blocking) {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete storage pool", nil)
		return
	}
	if imageCount := blocking.Resources["storage_images"]; imageCount > 0 {
		resources := map[string]int64{"storage_images": imageCount}
		if diskCount := blocking.Resources["vm_disks"]; diskCount > 0 {
			resources["vm_disks"] = diskCount
		}
		response.WriteBlockingResources(w, r, &response.BlockingResourcesError{
			Code:      response.CodeResourceInUse,
			Kind:      "pool",
			Message:   "storage pool still has materialised storage images; delete them first",
			Resources: resources,
		})
		return
	}
	response.WriteBlockingResources(w, r, &response.BlockingResourcesError{
		Message:   "storage pool is in use by virtual machine disks; remove or migrate them first",
		Resources: map[string]int64{"vm_disks": blocking.Resources["vm_disks"]},
	})
}
