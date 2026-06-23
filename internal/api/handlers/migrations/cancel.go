// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Cancel implements POST /v1/migrations/{id}/cancel - sync (200) with the
// current Migration view. Best-effort and valid only pre-cutover:
// PinnedNodeID is never touched, so a cancel is trivially fail-safe-to-source.
// Required permission: vm:migrate (a cancel mutates the migration, same gate as
// initiating it), held only by admin and operator (both scope any); developer
// and viewer have no vm:migrate and are rejected by RequirePermission (403)
// before this handler runs. A caller who can cancel but cannot otherwise see the
// migration gets 404 (no existence leak).
//
// An already-terminal migration (completed / failed / cancelled) is returned
// unchanged at 200 - cancel is idempotent and best-effort, mirroring the
// cordon/uncordon no-op precedent. Genuinely missing migrations return 404.
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserFromContext(r.Context())
	if caller == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "missing principal", nil)
		return
	}

	m, ok := h.resolveMigration(w, r)
	if !ok {
		return
	}

	// Ownership gate on vm:migrate: load the owning VM, deny cross-owner with
	// 404 (no leak). vm:migrate is operator+ (ScopeAny) in the matrix, so the
	// VM load is the only cross-owner branch in practice.
	vm, err := h.store.VMByID(r.Context(), m.VmID)
	if err != nil {
		// The owning VM vanished (force-delete race): treat as invisible.
		writeMigrationNotFound(w, r)
		return
	}
	if err := auth.CheckOwnership(caller, &vm.OwnerID, auth.PermVMMigrate); err != nil {
		writeMigrationNotFound(w, r)
		return
	}

	// Record WHO cancelled in a human-readable form: the caller's display name,
	// not the opaque principal UUID (JWTs carry only the user id, so resolve it)
	// and not the email (PII that would be exposed on the migration record to
	// every reader). Fall back to the id when the display name is unset or the
	// user vanished (soft-deleted mid-request) - the cancel must not fail on a
	// cosmetic lookup.
	cancelledBy := caller.ID.String()
	if u, uerr := h.store.UserByID(r.Context(), caller.ID); uerr == nil && u.DisplayName != "" {
		cancelledBy = u.DisplayName
	}
	updated, err := h.store.CancelMigration(r.Context(), m.ID, "cancelled by "+cancelledBy)
	if err != nil {
		if errors.Is(err, store.ErrMigrationNotCancelable) {
			// Already terminal: best-effort, return the current row unchanged at 200.
			response.WriteJSON(w, r, http.StatusOK, h.viewWithNames(r.Context(), updated))
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeMigrationNotFound(w, r)
			return
		}
		h.log.ErrorContext(r.Context(), "migrations.cancel failed",
			"migration_id", m.ID.String(), "error", err)
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "cancel migration", nil)
		return
	}

	// The migration is now authoritatively cancelled (phase=cancelled, terminal,
	// committed under a ModRevision CAS) - that is the fail-safe state and the
	// client's outcome. Best-effort, accelerate the agents' teardown on top of it:
	// tell the SOURCE to abort its outgoing push (so its outgoing task goes
	// terminal and the worker's PollTask unblocks) and, for a live migration with a
	// bound target, tell the TARGET to reap its incoming setup (so it does not leak
	// until the agent's 30-minute timeout). A slow / unreachable agent must never
	// hold the sync 200 hostage, and an agent failure must never change the
	// migration outcome - the agent's own timeout plus the worker convergence are
	// the backstops.
	h.propagateCancel(r.Context(), updated, vm)

	response.WriteJSON(w, r, http.StatusOK, h.viewWithNames(r.Context(), updated))
}

// propagateCancel best-effort tells the source (always) and the live-migration
// target (when bound) agents to abort, so a CP-side cancel does not leave the
// source mirroring and the target incoming leaking until their own timeouts. It
// is purely additive: every failure is logged at WARN and swallowed, and a
// bounded timeout keeps a slow agent from holding the sync response. A nil agent
// seam (agent router / tests) or a migration with no bound source skips it.
func (h *Handler) propagateCancel(ctx context.Context, m store.Migration, vm store.VM) {
	if h.agent == nil {
		return
	}

	abortCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if m.SourceNodeID != nil {
		if source, err := h.store.NodeByID(abortCtx, *m.SourceNodeID); err != nil {
			h.log.WarnContext(ctx, "migrations.cancel: resolve source node for propagation",
				"migration_id", m.ID.String(), "source_node_id", m.SourceNodeID.String(), "error", err)
		} else if _, err := h.agent.CancelMigration(abortCtx, source.AdvertisedEndpoint, vm.Name, m.ID.String()); err != nil {
			h.log.WarnContext(ctx, "migrations.cancel: source abort failed; relying on agent timeout + worker convergence",
				"migration_id", m.ID.String(), "endpoint", source.AdvertisedEndpoint, "error", err)
		}
	}

	// Only a LIVE target has an autonomous incoming-resume goroutine to unblock; an
	// offline target has nothing to reap on a pre-cutover cancel.
	if m.Live && m.TargetNodeID != nil {
		if target, err := h.store.NodeByID(abortCtx, *m.TargetNodeID); err != nil {
			h.log.WarnContext(ctx, "migrations.cancel: resolve target node for propagation",
				"migration_id", m.ID.String(), "target_node_id", m.TargetNodeID.String(), "error", err)
		} else if _, err := h.agent.CancelMigration(abortCtx, target.AdvertisedEndpoint, vm.Name, m.ID.String()); err != nil {
			h.log.WarnContext(ctx, "migrations.cancel: target incoming teardown failed; relying on agent timeout backstop",
				"migration_id", m.ID.String(), "endpoint", target.AdvertisedEndpoint, "error", err)
		}
	}
}
