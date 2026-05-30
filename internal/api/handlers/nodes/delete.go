// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// blockingResourcesError is raised inside the InTx closure when a
// non-force DELETE finds VMs or active migrations on the node. The
// outer handler unwraps it via errors.As and turns it into 409 with
// the standard `details.blocking_resources` payload.
type blockingResourcesError struct {
	VMs              int64
	ActiveMigrations int64
}

func (e *blockingResourcesError) Error() string {
	return fmt.Sprintf("node has %d vms and %d active migrations", e.VMs, e.ActiveMigrations)
}

// deleteResult carries observability counters out of the transaction.
// Only force-delete fills them; the non-force path leaves zeros.
type deleteResult struct {
	VMsOrphaned         int64
	MigrationsCancelled int64
}

// Delete implements DELETE /v1/nodes/{id}. Required permission:
// node:manage (admin only). {id} is name-only; UUID literals are
// rejected with 400 validation_failed at the resolver. Without
// `?force=true`, refuses with 409 when the node still hosts
// vm_runtime rows or has active migrations. With `?force=true`,
// runs in a transaction:
//
//  1. cancel every active migration touching the node (source or target);
//  2. orphan every vm_runtime row currently on the node — set
//     `phase = 'orphaned'` and clear `current_node_id`. Per migration
//     00003 vms.desired_phase is intentionally NOT touched: the user
//     still wants whatever they wanted before, the runtime just can't
//     deliver it;
//  3. soft-delete the node row.
//
// Force-delete is logged at INFO with node_id, user_id, vms_orphaned,
// migrations_cancelled, force=true. A dedicated audit-log subsystem is
// scheduled separately.
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

	var res deleteResult
	err = h.store.InTx(r.Context(), func(q *store.Queries) error {
		out, ferr := runDelete(r.Context(), q, node.ID, force, caller.ID)
		res = out
		return ferr
	})
	if err != nil {
		writeDeleteError(w, r, err)
		return
	}

	if force {
		h.log.InfoContext(r.Context(), "force-deleted node",
			slog.String("node_id", node.ID.String()),
			slog.String("user_id", caller.ID.String()),
			slog.Int64("vms_orphaned", res.VMsOrphaned),
			slog.Int64("migrations_cancelled", res.MigrationsCancelled),
			slog.Bool("force", true),
		)
	}

	response.WriteNoContent(w)
}

// runDelete is the transactional body of Delete. It returns the
// blockingResourcesError when the precheck fails, or any underlying
// DB error otherwise. force triggers the cancel-then-orphan sweep.
// Row-missing cases are caught upstream by the resolver-driven
// Delete handler.
func runDelete(ctx context.Context, q *store.Queries, id uuid.UUID, force bool, callerID uuid.UUID) (deleteResult, error) {
	vmCount, err := q.CountVMRuntimeOnNode(ctx, id)
	if err != nil {
		return deleteResult{}, err
	}
	migCount, err := q.CountActiveMigrationsOnNode(ctx, id)
	if err != nil {
		return deleteResult{}, err
	}

	if !force && (vmCount > 0 || migCount > 0) {
		return deleteResult{}, &blockingResourcesError{VMs: vmCount, ActiveMigrations: migCount}
	}

	var res deleteResult
	if force {
		reason := fmt.Sprintf("source/target node %s force-deleted by user %s", id, callerID)
		cancelled, err := q.CancelActiveMigrationsOnNode(ctx, store.CancelActiveMigrationsOnNodeParams{
			Reason: reason,
			NodeID: id,
		})
		if err != nil {
			return deleteResult{}, err
		}
		res.MigrationsCancelled = cancelled

		orphaned, err := q.OrphanVMRuntimeOnNode(ctx, id)
		if err != nil {
			return deleteResult{}, err
		}
		res.VMsOrphaned = orphaned
	}

	return res, q.SoftDeleteNode(ctx, id)
}

// writeDeleteError maps the in-flight error returned by runDelete to
// the standard envelope. Keeps Delete() under the gocyclo budget.
func writeDeleteError(w http.ResponseWriter, r *http.Request, err error) {
	var blocking *blockingResourcesError
	switch {
	case errors.As(err, &blocking):
		response.WriteError(w, r, http.StatusConflict,
			response.CodeConflict,
			"node has active resources; use force=true to detach them",
			map[string]any{
				"blocking_resources": map[string]int64{
					"vms":               blocking.VMs,
					"active_migrations": blocking.ActiveMigrations,
				},
				"force_available": true,
			})
	default:
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete node", nil)
	}
}
