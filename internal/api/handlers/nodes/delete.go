// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Delete implements DELETE /v1/nodes/{id}. Required permission:
// node:manage (admin only). {id} is name-only; UUID literals are
// rejected with 400 validation_failed at the resolver. Without
// `?force=true`, refuses with 409 when the node still hosts
// vm_runtime rows or has active migrations. With `?force=true`,
// store.DeleteNode runs the cancel-migrations + orphan-vms + soft-delete
// transaction (see its doc); vms.desired_phase is intentionally NOT
// touched.
//
// Every delete is logged at INFO with node_id, user_id, vms_orphaned,
// migrations_cancelled, certs_revoked, and the force bool. Both paths revoke
// the node's agent certs, so certs_revoked is recorded on the plain delete too;
// vms_orphaned / migrations_cancelled are zero unless force ran the detach
// cascade. A dedicated audit-log subsystem is scheduled separately.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	node, err := resolver.Node(r.Context(), h.store, chi.URLParam(r, "id"))
	if err != nil {
		writeNodeResolveError(w, r, err)
		return
	}

	force := r.URL.Query().Get("force") == "true"

	res, err := h.store.DeleteNode(r.Context(), node.ID, force, caller.ID)
	if err != nil {
		writeDeleteError(w, r, err)
		return
	}

	h.log.InfoContext(r.Context(), "deleted node",
		slog.String("node_id", node.ID.String()),
		slog.String("user_id", caller.ID.String()),
		slog.Int64("vms_orphaned", res.VMsOrphaned),
		slog.Int64("migrations_cancelled", res.MigrationsCancelled),
		slog.Int64("certs_revoked", res.CertsRevoked),
		slog.Bool("force", force),
	)

	response.WriteNoContent(w)
}

// writeDeleteError maps the error returned by store.DeleteNode to the
// standard envelope. The blocking case carries both resource counts
// (vms and active_migrations) plus force_available=true so the client
// knows the detach path exists.
func writeDeleteError(w http.ResponseWriter, r *http.Request, err error) {
	var inUse *store.ResourceInUseError
	switch {
	case errors.As(err, &inUse):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict,
			"node has active resources; use force=true to detach them",
			map[string]any{
				"blocking_resources": map[string]int64{
					"vms":               inUse.Resources["vms"],
					"active_migrations": inUse.Resources["active_migrations"],
				},
				"force_available": true,
			})
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete node", nil)
	}
}
