// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package templates

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// errTemplateNotFound is the in-flight signal that the row is missing
// or invisible. Mapped to 404 by the outer handler.
var errTemplateNotFound = errors.New("template not found")

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

	row, err := resolver.Template(r.Context(), h.store.Queries(), chi.URLParam(r, "id"))
	if err != nil {
		writeLoadError(w, r, err)
		return
	}

	err = h.store.InTx(r.Context(), func(q *store.Queries) error {
		return runDelete(r.Context(), q, row, caller)
	})
	if err != nil {
		writeDeleteError(w, r, err)
		return
	}

	response.WriteNoContent(w)
}

// runDelete is the transactional body of Delete. It returns
// errTemplateNotFound when the caller may not see the row, a
// *response.BlockingResourcesError when dependent resources block
// the delete, or any underlying DB error otherwise. The row is
// resolved upstream so the transaction body stays focused on
// authorization + cascade prechecks + soft-delete.
func runDelete(ctx context.Context, q *store.Queries, row store.Template, caller *auth.User) error {
	if err := auth.CheckOwnership(caller, &row.OwnerID, auth.PermTemplateDelete); err != nil {
		if errors.Is(err, auth.ErrPermissionDenied) {
			return errTemplateNotFound
		}
		return err
	}
	vmCount, err := q.CountActiveVMsForTemplate(ctx, row.ID)
	if err != nil {
		return err
	}
	imageCount, err := q.CountStorageImagesByTemplate(ctx, row.ID)
	if err != nil {
		return err
	}
	// Stacked refusal. Storage images carry the new endpoint-specific
	// code; the vm-only branch keeps the generic `conflict` to preserve
	// the pre-existing wire contract.
	if imageCount > 0 {
		resources := map[string]int64{"storage_images": imageCount}
		if vmCount > 0 {
			resources["vms"] = vmCount
		}
		return &response.BlockingResourcesError{
			Code:      response.CodeResourceInUse,
			Kind:      "template",
			Message:   "template still has materialised storage images; delete them first",
			Resources: resources,
		}
	}
	if vmCount > 0 {
		return &response.BlockingResourcesError{
			Message:   "template is in use; remove or migrate the dependent virtual machines first",
			Resources: map[string]int64{"vms": vmCount},
		}
	}
	return q.SoftDeleteTemplate(ctx, row.ID)
}

// writeDeleteError maps the in-flight error returned by runDelete to
// the standard envelope.
func writeDeleteError(w http.ResponseWriter, r *http.Request, err error) {
	var blocking *response.BlockingResourcesError
	switch {
	case errors.Is(err, errTemplateNotFound):
		writeNotFound(w, r)
	case errors.As(err, &blocking):
		response.WriteBlockingResources(w, r, blocking)
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete template", nil)
	}
}
