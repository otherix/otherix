// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package templates

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	storagepoolshandlers "github.com/otherix/otherix/internal/api/handlers/storagepools"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// imageImportRequest is the body of POST /v1/templates/{id}/images.
// The handler intentionally restricts the surface to `pool`; the
// agent-side ImageImportRequest (source_url / expected_checksum_sha256
// / format) is composed from the template row by the worker so
// callers cannot materialise content that does not match the
// template's advertised image.
//
// `pool` accepts either a UUID literal or a storage-pool name. The
// resolver layer normalises both.
type imageImportRequest struct {
	Pool string `json:"pool"`
}

// errImageImportForbidden is the in-flight signal that the composite
// RBAC check (storage_image:import scope=any OR scope=own + owned OR
// template:read:public + visibility=public) rejected the request.
// Mapped to 403 permission_denied — storage_images existence is
// observable via image_cache:read so 404 here would lie about a row
// the caller can already see.
var errImageImportForbidden = errors.New("storage image import forbidden")

// errPoolUnsupported is the sentinel returned by checkPoolType when
// the pool's type is not in the importable set. Only `local_dir` is
// currently importable; the helper exists primarily for unit tests
// and as a defence-in-depth surface if a future migration loosens
// the DB check constraint.
var errPoolUnsupported = errors.New("pool type unsupported")

// ImportImage implements POST /v1/templates/{id}/images. Atomic
// enqueue of a `storage_image.import` Task + river job +
// UpdateTaskRiverJobID inside one transaction. Returns
// 202 + AsyncTaskAccepted; clients poll /v1/tasks/{task_id} for
// completion.
//
// {id} is a template name (UUID literals rejected with 400
// validation_failed). The request body's `pool` field stays
// polymorphic per the multi-instance carve-out: either a pool name
// or a per-instance UUID literal.
//
// Permission: `storage_image:import`. The route gate
// (RequirePermission) admits any role holding the permission at any
// scope; the composite ownership / public-bypass check
// (checkImportAccess) fires inside the handler after the template is
// loaded.
func (h *Handler) ImportImage(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	poolIdentifier, ok := decodeImageImportBody(w, r)
	if !ok {
		return
	}

	template, err := resolver.Template(r.Context(), h.store.Queries(), chi.URLParam(r, "id"))
	if err != nil {
		writeLoadError(w, r, err)
		return
	}
	if err := checkImportAccess(caller, template); err != nil {
		response.WriteError(w, r, http.StatusForbidden,
			response.CodePermissionDenied,
			"caller may not import storage images for this template",
			map[string]any{"required_permission": string(auth.PermStorageImageImport)})
		return
	}

	pool, err := resolver.Pool(r.Context(), h.store.Queries(), poolIdentifier)
	if err != nil {
		if resolver.IsNotFound(err) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "storage pool not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load storage pool", nil)
		return
	}
	if err := checkPoolType(pool); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed,
			"unsupported pool type for import",
			map[string]any{"pool_type": pool.Type})
		return
	}

	node, err := h.store.Queries().GetNodeByID(r.Context(), pool.NodeID)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load owning node", nil)
		return
	}
	if !nodeIsScannable(node.Status) {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict, "node is not in a scannable state",
			map[string]any{"current_status": string(node.Status)})
		return
	}

	taskID, err := h.enqueueImport(r.Context(), template.ID, pool.ID, caller.ID)
	if err != nil {
		h.log.ErrorContext(r.Context(), "templates.imageImport enqueue failed",
			"template_id", template.ID, "pool_id", pool.ID, "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "enqueue import", nil)
		return
	}

	response.WriteJSON(w, r, http.StatusAccepted, response.AsyncTaskAccepted{
		TaskID: taskID.String(),
		Status: string(store.TaskStatusPending),
		Links:  response.AsyncTaskLinks{Self: "/v1/tasks/" + taskID.String()},
	})
}

// decodeImageImportBody reads the request body, validates the `pool`
// field, and writes the appropriate 400 envelope on failure. Returns
// the raw identifier (UUID literal or pool name) and ok=true on
// success; UUID vs name semantics are decided by the resolver layer.
func decodeImageImportBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body imageImportRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid request body", nil)
		return "", false
	}
	if body.Pool == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "pool is required", nil)
		return "", false
	}
	return body.Pool, true
}

// enqueueImport runs the atomic three-write enqueue: insert the
// task row, hand the job to river inside the same tx, then stamp
// the river job id back. Mirrors the storage_pool.scan handler's
// enqueueScan exactly — the only differences are the task type and
// the args payload shape.
func (h *Handler) enqueueImport(ctx context.Context, templateID, poolID, callerID uuid.UUID) (uuid.UUID, error) {
	taskID := uuid.New()
	tid := templateID
	cid := callerID

	argsJSON, err := json.Marshal(map[string]any{
		"template_id": templateID.String(),
		"pool_id":     poolID.String(),
	})
	if err != nil {
		return uuid.Nil, err
	}

	err = h.store.InTxWithTx(ctx, func(q *store.Queries, tx pgx.Tx) error {
		if _, err := q.CreateTask(ctx, store.CreateTaskParams{
			ID:           taskID,
			Type:         "storage_image.import",
			Status:       store.TaskStatusPending,
			ResourceType: "template",
			ResourceID:   &tid,
			Args:         argsJSON,
			MaxAttempts:  25,
			CreatedBy:    &cid,
		}); err != nil {
			return err
		}
		insertResult, err := h.riverClient.InsertTx(ctx, tx,
			storagepoolshandlers.StorageImageImportArgs{
				TaskID:     taskID,
				TemplateID: templateID,
				PoolID:     poolID,
			}, nil)
		if err != nil {
			return err
		}
		jobID := insertResult.Job.ID
		return q.UpdateTaskRiverJobID(ctx, store.UpdateTaskRiverJobIDParams{
			ID:         taskID,
			RiverJobID: &jobID,
		})
	})
	if err != nil {
		return uuid.Nil, err
	}
	return taskID, nil
}

// checkImportAccess enforces the composite ownership rule:
//
//  1. caller holds storage_image:import at scope='any' (admin /
//     operator), OR
//  2. caller holds the permission at scope='own' AND
//     template.owner_id == caller.id, OR
//  3. caller holds template:read:public AND
//     template.visibility == 'public'.
//
// Otherwise errImageImportForbidden → 403 permission_denied (read
// endpoints openly observable; 404 would lie).
func checkImportAccess(caller *auth.User, template store.Template) error {
	scope := auth.ScopeFor(caller.Role, auth.PermStorageImageImport)
	if scope == auth.ScopeAny {
		return nil
	}
	if scope == auth.ScopeOwn && template.OwnerID == caller.ID {
		return nil
	}
	if auth.Has(caller.Role, auth.PermTemplateReadPublic) && template.Visibility == "public" {
		return nil
	}
	return errImageImportForbidden
}

// checkPoolType validates the pool's type against the importable
// set. Currently only `local_dir` is supported; the DB check
// constraint enforces the same value at the storage layer, so this
// helper exists primarily as a defence-in-depth seam for unit
// tests and to surface a structured 400 envelope if a future
// migration loosens the constraint.
func checkPoolType(pool store.StoragePool) error {
	if pool.Type == "local_dir" {
		return nil
	}
	return errPoolUnsupported
}

// nodeIsScannable mirrors the scan handler's inline check
// (storagepools/scan.go) — pending / ready / cordoned nodes accept
// new agent-driven work; everything else is rejected with 409.
func nodeIsScannable(status store.NodeStatus) bool {
	switch status {
	case store.NodeStatusPending, store.NodeStatusReady, store.NodeStatusCordoned:
		return true
	}
	return false
}
