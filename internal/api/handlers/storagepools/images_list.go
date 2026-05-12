// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/pagination"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// ListImages implements GET /v1/storage-pools/{pool_id}/images. Required
// permission: `image_cache:read` (held by every authenticated role per
// docs/rbac.md). No scope filtering — storage_images are an
// infrastructure projection and are visible to every reader of the
// owning pool. {pool_id} accepts either a UUID literal or a pool name.
//
// Cursor pagination keyed on (imported_at, id) DESC. The
// pagination.Cursor type names its timestamp slot CreatedAt because the
// helper is generic across resources; for storage_images the slot
// carries imported_at.
func (h *Handler) ListImages(w http.ResponseWriter, r *http.Request) {
	pool, err := resolver.Pool(r.Context(), h.store.Queries(), chi.URLParam(r, "pool_id"))
	if err != nil {
		writePoolResolveError(w, r, err, "storage pool not found", "load storage pool")
		return
	}

	q := r.URL.Query()
	limit := pagination.Limit(pagination.ParseLimit(q.Get("limit")))
	cur, err := pagination.Decode(q.Get("cursor"))
	if err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "invalid cursor", nil)
		return
	}

	params := store.ListStorageImagesByPoolParams{
		PoolID:     pool.ID,
		LimitCount: limit + 1,
	}
	if cur != nil {
		ts := cur.CreatedAt
		id := cur.ID
		params.CursorImportedAt = &ts
		params.CursorID = &id
	}

	rows, err := h.store.Queries().ListStorageImagesByPool(r.Context(), params)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "list storage images", nil)
		return
	}

	limitN := int(limit)
	var nextCursor *string
	if len(rows) > limitN {
		last := rows[limitN-1]
		s := pagination.Encode(&pagination.Cursor{CreatedAt: last.ImportedAt, ID: last.ID})
		nextCursor = &s
		rows = rows[:limitN]
	}

	views := make([]storageImageView, 0, len(rows))
	for _, im := range rows {
		view, err := h.projectImageView(r.Context(), im, pool.Name)
		if err != nil {
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "project storage image", nil)
			return
		}
		views = append(views, view)
	}
	response.WriteJSON(w, r, http.StatusOK, storageImageListResponse{
		Data: views,
		Meta: paginationMeta{NextCursor: nextCursor},
	})
}

// projectImageView builds the public storageImageView для a row.
// poolName is supplied by the caller (already resolved from URL).
// Template name comes through a GetTemplate lookup; a soft-deleted
// template materialises as an empty string rather than 500ing the
// listing — orphan storage images are still observable.
func (h *Handler) projectImageView(ctx context.Context, im store.StorageImage, poolName string) (storageImageView, error) {
	templateName := ""
	tmpl, err := h.store.Queries().GetTemplate(ctx, im.TemplateID)
	switch {
	case err == nil:
		templateName = tmpl.Name
	case errors.Is(err, pgx.ErrNoRows):
		// keep templateName empty — see godoc.
	default:
		return storageImageView{}, err
	}
	return toImageView(im, templateName, poolName), nil
}

// toImageView projects a store.StorageImage onto its public
// storageImageView, substituting the pre-resolved template и pool
// names. ImportedAt formats in RFC 3339 nanoseconds in UTC, matching
// the rest of the codebase.
func toImageView(im store.StorageImage, templateName, poolName string) storageImageView {
	return storageImageView{
		ID:             im.ID.String(),
		Template:       templateName,
		Pool:           poolName,
		ChecksumSHA256: im.ChecksumSha256,
		SizeBytes:      im.SizeBytes,
		Format:         im.Format,
		ImportedAt:     im.ImportedAt.UTC().Format(time.RFC3339Nano),
	}
}
