// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

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

	err = h.store.InTx(r.Context(), func(q *store.Queries) error {
		return runDelete(r.Context(), q, row.ID)
	})
	if err != nil {
		writeDeleteError(w, r, err)
		return
	}

	response.WriteNoContent(w)
}

// runDelete is the transactional body of Delete. It returns a
// *response.BlockingResourcesError when active vm_disks or
// materialised storage images block the delete, or any underlying DB
// error otherwise. Row-missing cases are caught upstream by the
// resolver-driven Delete handler.
func runDelete(ctx context.Context, q *store.Queries, id uuid.UUID) error {
	diskCount, err := q.CountVMDisksOnStoragePool(ctx, id)
	if err != nil {
		return err
	}
	imageCount, err := q.CountStorageImagesByPool(ctx, id)
	if err != nil {
		return err
	}
	// Stacked refusal. Storage images carry the
	// endpoint-specific code; the vm_disks-only branch keeps the
	// generic `conflict` to preserve the historical wire contract.
	if imageCount > 0 {
		resources := map[string]int64{"storage_images": imageCount}
		if diskCount > 0 {
			resources["vm_disks"] = diskCount
		}
		return &response.BlockingResourcesError{
			Code:      response.CodeResourceInUse,
			Kind:      "pool",
			Message:   "storage pool still has materialised storage images; delete them first",
			Resources: resources,
		}
	}
	if diskCount > 0 {
		return &response.BlockingResourcesError{
			Message:   "storage pool is in use by virtual machine disks; remove or migrate them first",
			Resources: map[string]int64{"vm_disks": diskCount},
		}
	}

	return q.SoftDeleteStoragePool(ctx, id)
}

// writeDeleteError maps the in-flight error returned by runDelete to
// the standard envelope.
func writeDeleteError(w http.ResponseWriter, r *http.Request, err error) {
	var blocking *response.BlockingResourcesError
	switch {
	case errors.As(err, &blocking):
		response.WriteBlockingResources(w, r, blocking)
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete storage pool", nil)
	}
}
