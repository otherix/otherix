// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import (
	"net/http"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Get implements GET /v1/migrations/{id}. Required permission: vm:read (the
// migration is observed runtime state of a VM, so a vm:read holder can poll
// it). The {id} param is either a full migration UUID or a unique short hex
// prefix of it (git/docker style): an exact match resolves, an ambiguous prefix
// is 409, a too-short / non-hex prefix is 400. See resolveMigration.
//
// Visibility: every authenticated role holds vm:read at ScopeAny today, so the
// scope gate is a no-op in practice; it is kept so a future ScopeOwn vm:read
// role cannot read another owner's migration. A ScopeOwn caller whose id does
// not match the owning VM's owner gets 404 (not 403, per the no-existence-leak
// rule). Genuinely missing migrations also return 404; the two share an
// envelope so callers cannot distinguish "invisible" from "absent".
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
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

	if !h.callerCanSeeMigration(r, caller, m) {
		writeMigrationNotFound(w, r)
		return
	}

	response.WriteJSON(w, r, http.StatusOK, h.viewWithNames(r.Context(), m))
}

// callerCanSeeMigration applies the vm:read scope gate: ScopeAny sees every
// migration; ScopeOwn sees only migrations whose owning VM the caller owns;
// anything else is invisible. A failure to load the owning VM is treated as
// invisible (no-leak fall-through). The bool result feeds a 404 envelope.
func (h *Handler) callerCanSeeMigration(r *http.Request, caller *auth.User, m store.Migration) bool {
	switch auth.ScopeFor(caller.Role, auth.PermVMRead) {
	case auth.ScopeAny:
		return true
	case auth.ScopeOwn:
		vm, err := h.store.VMByID(r.Context(), m.VmID)
		if err != nil {
			return false
		}
		return vm.OwnerID == caller.ID
	default:
		return false
	}
}

func writeMigrationNotFound(w http.ResponseWriter, r *http.Request) {
	response.WriteError(w, r, http.StatusNotFound,
		response.CodeNotFound, "migration not found", nil)
}
