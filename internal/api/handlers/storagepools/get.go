// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// Get implements GET /v1/storage-pools/{id}. Required permission:
// storage_pool:read.
//
// {id} is polymorphic. A UUID literal returns the single per-node
// storage_pool instance row (flat StoragePool view); a name returns
// the aggregated PoolConceptView with every per-node instance plus the
// cluster-default flag. The two shapes are surfaced via a `oneOf` on
// the OpenAPI side — the mental model is StorageClass (name) vs
// PersistentVolume (instance UUID).
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	identifier := chi.URLParam(r, "id")
	if _, err := uuid.Parse(identifier); err == nil {
		h.getByID(w, r, identifier)
		return
	}
	h.getByName(w, r, identifier)
}

func (h *Handler) getByID(w http.ResponseWriter, r *http.Request, identifier string) {
	row, err := resolver.Pool(r.Context(), h.store, identifier)
	if err != nil {
		writePoolResolveError(w, r, err, "storage pool not found", "load storage pool")
		return
	}
	view, err := h.projectView(r.Context(), row)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load storage pool", nil)
		return
	}
	response.WriteJSON(w, r, http.StatusOK, view)
}

func (h *Handler) getByName(w http.ResponseWriter, r *http.Request, name string) {
	concept, err := resolver.PoolByName(r.Context(), h.store, name)
	if err != nil {
		writePoolResolveError(w, r, err, "storage pool not found", "load storage pool")
		return
	}
	// Re-fetch every instance from the effective view so each rendered
	// row carries available_bytes_effective. resolver.PoolByName uses
	// the raw table to stay backwards-compatible with the per-instance
	// disambiguation logic; the view re-read closes the loop here.
	rows, err := h.store.ListPoolsEffectiveByName(r.Context(), name)
	if err != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load storage pool", nil)
		return
	}
	instances := make([]storagePoolView, 0, len(rows))
	for _, row := range rows {
		view, err := h.projectInstanceForConcept(r.Context(), row, concept.IsClusterDefault)
		if err != nil {
			response.WriteError(w, r, http.StatusInternalServerError,
				response.CodeInternal, "load storage pool", nil)
			return
		}
		instances = append(instances, view)
	}
	response.WriteJSON(w, r, http.StatusOK, poolConceptView{
		Name:             concept.Name,
		Type:             concept.Type,
		IsClusterDefault: concept.IsClusterDefault,
		Instances:        instances,
	})
}

// projectInstanceForConcept reuses the per-row projector but skips a
// second cluster_settings round-trip — the aggregated view already knows
// the cluster-default flag.
func (h *Handler) projectInstanceForConcept(ctx context.Context, p store.PoolEffectiveCapacity, isClusterDefault bool) (storagePoolView, error) {
	node, err := h.store.NodeByID(ctx, p.NodeID)
	if err != nil {
		// Aligned with projectView: a soft-deleted owning node yields an
		// empty Node field rather than a hard error, so the operator
		// can still see the instance in the aggregated view.
		node = store.Node{}
	}
	return toViewEffective(p, node.Name, node.Status, isClusterDefault), nil
}
