// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package networks

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// errNetworkNotFound is the in-flight signal that the target row is
// missing or already soft-deleted. Mapped to 404 by the outer handler.
var errNetworkNotFound = errors.New("network not found")

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

	err = h.store.InTx(r.Context(), func(q *store.Queries) error {
		return runDelete(r.Context(), q, id)
	})
	if err != nil {
		writeDeleteError(w, r, err)
		return
	}

	response.WriteNoContent(w)
}

// runDelete is the transactional body of Delete. It returns
// errNetworkNotFound when the row is missing, a
// *response.BlockingResourcesError when active vm_nics block the
// delete, or any underlying DB error otherwise.
func runDelete(ctx context.Context, q *store.Queries, id uuid.UUID) error {
	if _, err := q.GetNetworkByID(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errNetworkNotFound
		}
		return err
	}

	nicCount, err := q.CountVMNicsOnNetwork(ctx, id)
	if err != nil {
		return err
	}
	if nicCount > 0 {
		return &response.BlockingResourcesError{
			Message:   "network is in use by virtual machine NICs; remove or migrate them first",
			Resources: map[string]int64{"vm_nics": nicCount},
		}
	}

	return q.SoftDeleteNetwork(ctx, id)
}

// writeDeleteError maps the in-flight error returned by runDelete to
// the standard envelope.
func writeDeleteError(w http.ResponseWriter, r *http.Request, err error) {
	var blocking *response.BlockingResourcesError
	switch {
	case errors.Is(err, errNetworkNotFound):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "network not found", nil)
	case errors.As(err, &blocking):
		response.WriteBlockingResources(w, r, blocking)
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete network", nil)
	}
}
