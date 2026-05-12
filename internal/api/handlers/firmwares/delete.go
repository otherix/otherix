// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package firmwares

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

// errFirmwareNotFound is the in-flight signal that the target row is
// missing. Mapped to 404 by the outer handler.
var errFirmwareNotFound = errors.New("firmware not found")

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
// errFirmwareNotFound when the row is missing, a
// *response.BlockingResourcesError when active vms or templates block
// the delete, or any underlying DB error otherwise.
func runDelete(ctx context.Context, q *store.Queries, id uuid.UUID) error {
	if _, err := q.GetFirmwareByID(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errFirmwareNotFound
		}
		return err
	}

	vmCount, err := q.CountVMsForFirmware(ctx, id)
	if err != nil {
		return err
	}
	tplCount, err := q.CountTemplatesForFirmware(ctx, id)
	if err != nil {
		return err
	}
	if vmCount > 0 || tplCount > 0 {
		blocking := map[string]int64{}
		if vmCount > 0 {
			blocking["vms"] = vmCount
		}
		if tplCount > 0 {
			blocking["templates"] = tplCount
		}
		return &response.BlockingResourcesError{
			Message:   "firmware is in use; remove the dependent resources first",
			Resources: blocking,
		}
	}

	return q.DeleteFirmware(ctx, id)
}

// writeDeleteError maps the in-flight error returned by runDelete to
// the standard envelope.
func writeDeleteError(w http.ResponseWriter, r *http.Request, err error) {
	var blocking *response.BlockingResourcesError
	switch {
	case errors.Is(err, errFirmwareNotFound):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "firmware not found", nil)
	case errors.As(err, &blocking):
		response.WriteBlockingResources(w, r, blocking)
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete firmware", nil)
	}
}
