// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package templates

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Delete implements DELETE /v1/templates/{id}. Required permission:
// `template:delete` (admin/operator: any; developer: own). Cross-user
// developer attempts surface as 404 (no leak), not 403. {id} is a
// template name; UUID literals are rejected with 400 validation_failed
// at the resolver.
//
// The block on active VMs is a deliberate API-level policy — three
// FKs reference templates with different ON DELETE semantics (vms:
// SET NULL, vm_disks: RESTRICT, node_image_cache: CASCADE). Soft-delete
// bypasses all three (the row physically remains), so the precheck
// here is the sole enforcement of "don't remove a template that's
// still in use". Force-delete is intentionally absent — the operator
// must remove or migrate the dependent VMs first.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	row, err := resolver.Template(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writeLoadError(w, r, err)
		return
	}

	// Ownership is a policy decision over the already-loaded row - it
	// does not touch the database - so it stays in the handler. A
	// cross-user developer attempt collapses to 404 (no existence leak).
	if err := auth.CheckOwnership(caller, &row.OwnerID, auth.PermTemplateDelete); err != nil {
		if errors.Is(err, auth.ErrPermissionDenied) {
			writeNotFound(w, r)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "ownership check", nil)
		return
	}

	if err := h.store.DeleteTemplate(r.Context(), row.ID); err != nil {
		writeDeleteError(w, r, err)
		return
	}

	response.WriteNoContent(w)
}

// writeDeleteError maps the store domain error to the standard envelope.
// A *store.ResourceInUseError carries the blocking counts; the handler
// owns the wire policy of how those counts become a 409 envelope. The
// storage-image branch carries the endpoint-specific resource_in_use
// code; the vm-only branch keeps the generic `conflict` to preserve the
// pre-existing wire contract.
func writeDeleteError(w http.ResponseWriter, r *http.Request, err error) {
	var blocking *store.ResourceInUseError
	if !errors.As(err, &blocking) {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete template", nil)
		return
	}
	if imageCount := blocking.Resources["storage_images"]; imageCount > 0 {
		resources := map[string]int64{"storage_images": imageCount}
		if vmCount := blocking.Resources["vms"]; vmCount > 0 {
			resources["vms"] = vmCount
		}
		response.WriteBlockingResources(w, r, &response.BlockingResourcesError{
			Code:      response.CodeResourceInUse,
			Kind:      "template",
			Message:   "template still has materialised storage images; delete them first",
			Resources: resources,
		})
		return
	}
	response.WriteBlockingResources(w, r, &response.BlockingResourcesError{
		Message:   "template is in use; remove or migrate the dependent virtual machines first",
		Resources: map[string]int64{"vms": blocking.Resources["vms"]},
	})
}
