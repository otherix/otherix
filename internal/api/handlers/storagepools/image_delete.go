// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// ImageDeleter is the narrow interface the storage_image.delete
// handler depends on (Go-style "consumer defines"). The production
// *agentclient.Client satisfies it structurally; tests inject a
// fake without importing the agent client at all.
type ImageDeleter interface {
	DeleteImage(
		ctx context.Context,
		endpoint string,
		poolName string,
		checksumSHA256 string,
		idempotencyKey string,
	) error
}

// errImageNotFound is the in-flight signal that the image row is
// missing or in a different pool. Mapped to 404 by the outer handler.
var errImageNotFound = errors.New("storage image not found")

// errImageDeleteForbidden is the in-flight signal that the composite
// RBAC check rejected the request after the role-level
// RequirePermission(storage_image:manage) middleware admitted it.
// Composite failure surfaces as 403 permission_denied -
// storage_images existence is observable via image_cache:read
// (every authenticated role) so 404 here would lie about a row the
// caller already saw via the list endpoint. This intentionally
// departs from templates/delete.go's 404 mapping because the
// visibility model differs: templates are read-gated by
// scope=own/public visibility, storage_images are read-open.
var errImageDeleteForbidden = errors.New("storage image delete forbidden")

// errAgentUnreachable is the in-flight signal that the agent's
// synchronous delete failed in a way that warrants rolling back the
// transaction (5xx, network error). 502 envelope at the wire.
var errAgentUnreachable = errors.New("agent unreachable on storage image delete")

// DeleteImage implements DELETE /v1/storage-pools/{pool_id}/images/{image_id}.
// Required permission: `storage_image:manage` (admin/operator any;
// developer own via composite ownership / public-bypass - see
// checkImageDeleteAccess). {pool_id} accepts either a UUID literal
// or a pool name; {image_id} stays UUID-only since storage_images
// carry no name.
//
// Refcount-gated: when other storage_images rows in the same pool
// share the checksum the agent's delete is NOT invoked - the
// on-disk file stays for the surviving siblings. Only the
// last-referent delete invokes the agent inside the same SQL
// transaction so the row removal and the file removal commit
// atomically. The InTx duration is sub-second on the happy path
// because agent-side work is local filesystem (chattr -i +
// os.Remove).
func (h *Handler) DeleteImage(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	pool, err := resolver.Pool(r.Context(), h.store.Queries(), chi.URLParam(r, "pool_id"))
	if err != nil {
		writePoolResolveError(w, r, err, "storage pool not found", "load storage pool")
		return
	}
	imageID, err := uuid.Parse(chi.URLParam(r, "image_id"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "image_id must be a uuid", nil)
		return
	}

	var deletedView storageImageView
	err = h.store.InTx(r.Context(), func(q *store.Queries) error {
		view, err := h.runImageDelete(r.Context(), q, caller, pool, imageID)
		if err != nil {
			return err
		}
		deletedView = view
		return nil
	})
	if err != nil {
		writeImageDeleteError(w, r, err)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, deletedView)
}

// runImageDelete is the transactional body of DeleteImage. Returns
// errImageNotFound, errImageDeleteForbidden, errAgentUnreachable,
// or any underlying DB error.
func (h *Handler) runImageDelete(
	ctx context.Context,
	q *store.Queries,
	caller *auth.User,
	pool store.StoragePool,
	imageID uuid.UUID,
) (storageImageView, error) {
	image, err := q.GetStorageImageByID(ctx, imageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return storageImageView{}, errImageNotFound
		}
		return storageImageView{}, fmt.Errorf("get storage image: %w", err)
	}
	if image.PoolID != pool.ID {
		return storageImageView{}, errImageNotFound
	}

	template, err := q.GetTemplate(ctx, image.TemplateID)
	if err != nil {
		// FK-guaranteed; missing here means soft-deleted template.
		// Surface as 500 since the data model invariant is broken.
		if errors.Is(err, pgx.ErrNoRows) {
			return storageImageView{}, fmt.Errorf("template %s missing for image %s", image.TemplateID, imageID)
		}
		return storageImageView{}, fmt.Errorf("get template: %w", err)
	}
	if err := checkImageDeleteAccess(caller, template); err != nil {
		return storageImageView{}, err
	}

	count, err := q.CountStorageImagesBySHA256InPool(ctx, store.CountStorageImagesBySHA256InPoolParams{
		PoolID:         pool.ID,
		ChecksumSha256: image.ChecksumSha256,
		ExcludeID:      imageID,
	})
	if err != nil {
		return storageImageView{}, fmt.Errorf("count siblings: %w", err)
	}

	if count == 0 {
		if err := h.invokeAgentDelete(ctx, q, pool.ID, pool.Name, image.ChecksumSha256); err != nil {
			return storageImageView{}, err
		}
	}

	if err := q.DeleteStorageImage(ctx, imageID); err != nil {
		return storageImageView{}, fmt.Errorf("delete storage image row: %w", err)
	}
	return toImageView(image, template.Name, pool.Name), nil
}

// invokeAgentDelete loads the owning node and calls the agent's
// synchronous DeleteImage. 5xx / network failures and a missing
// agent client both map to errAgentUnreachable (502). agentclient's
// DeleteImage already collapses 204 / 404 to nil (idempotent), so a
// nil here is unconditional success.
func (h *Handler) invokeAgentDelete(
	ctx context.Context,
	q *store.Queries,
	poolID uuid.UUID,
	poolName string,
	checksum string,
) error {
	if h.imageDeleter == nil {
		return errAgentUnreachable
	}
	pool, err := q.GetStoragePoolByID(ctx, poolID)
	if err != nil {
		return fmt.Errorf("get pool for agent delete: %w", err)
	}
	node, err := q.GetNodeByID(ctx, pool.NodeID)
	if err != nil {
		return fmt.Errorf("get node for agent delete: %w", err)
	}
	idempotencyKey := uuid.NewString()
	if err := h.imageDeleter.DeleteImage(ctx, node.AdvertisedEndpoint, poolName, checksum, idempotencyKey); err != nil {
		var ae *agentclient.AgentError
		if errors.As(err, &ae) && !ae.IsRetryable() {
			// 4xx other than 404 (404 is collapsed inside agentclient):
			// non-retryable — but the CP-side row still cannot be
			// safely removed without confirming agent state. Surface
			// as agent_unreachable so the operator investigates; the
			// envelope's contents remain useful for audit logs.
			h.log.WarnContext(ctx, "agent storage_image delete returned non-retryable error",
				"pool_id", poolID, "pool_name", poolName, "checksum", checksum,
				"agent_status", ae.Status, "agent_code", ae.Code)
		}
		return errAgentUnreachable
	}
	return nil
}

// checkImageDeleteAccess enforces the composite ownership rule. The
// caller already holds storage_image:manage at SOME scope
// (RequirePermission middleware admitted them). Permitted iff:
//
//  1. scope='any' (admin / operator), OR
//  2. scope='own' AND template.owner_id == caller.id, OR
//  3. caller holds template:read:public AND template.visibility == 'public'.
//
// Else errImageDeleteForbidden → 403 permission_denied. This
// departs from templates/delete.go's 404 mapping by design - see
// errImageDeleteForbidden's godoc: storage_images are observable
// via the read-open list/get endpoints, so existence is not hidden
// and 404 here would lie about a row the caller can already see.
func checkImageDeleteAccess(caller *auth.User, template store.Template) error {
	scope := auth.ScopeFor(caller.Role, auth.PermStorageImageManage)
	if scope == auth.ScopeAny {
		return nil
	}
	if scope == auth.ScopeOwn && template.OwnerID == caller.ID {
		return nil
	}
	if auth.Has(caller.Role, auth.PermTemplateReadPublic) && template.Visibility == "public" {
		return nil
	}
	return errImageDeleteForbidden
}

// writeImageDeleteError maps the in-flight error returned by
// runImageDelete to the standard envelope.
func writeImageDeleteError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errImageNotFound):
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "storage image not found", nil)
	case errors.Is(err, errImageDeleteForbidden):
		response.WriteError(w, r, http.StatusForbidden,
			response.CodePermissionDenied,
			"caller may not delete this storage image",
			map[string]any{"required_permission": string(auth.PermStorageImageManage)})
	case errors.Is(err, errAgentUnreachable):
		response.WriteError(w, r, http.StatusBadGateway,
			response.CodeAgentUnreachable,
			"agent could not complete the storage image delete; transaction rolled back",
			nil)
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete storage image", nil)
	}
}
