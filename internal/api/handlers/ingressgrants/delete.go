// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ingressgrants

import (
	"errors"
	"net/http"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// Delete implements DELETE /v1/ingress-grants/{id}. Required permission:
// vm:ingress-grant. The caller must own the grant (else 404). Delete removes the
// grant row, its name guard, and its token index in one atomic transaction,
// freeing the name for reuse - the difference from revoke, which keeps the row
// (and the consumed name) for audit. A token for a deleted grant resolves to
// no row and is rejected uniformly at the cert/stream path, exactly as a miss.
// Returns 204 No Content.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	grant, ok := h.loadOwnedGrant(w, r)
	if !ok {
		return
	}

	if err := h.store.DeleteIngressGrant(r.Context(), grant.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeGrantNotFound(w, r)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "delete ingress grant", nil)
		return
	}

	response.WriteNoContent(w)
}
