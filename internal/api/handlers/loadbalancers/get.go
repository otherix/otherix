// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/store"
)

// Get implements GET /v1/loadbalancers/{id}, where {id} is the load-balancer
// NAME. Required permission: loadbalancer:read. The response surfaces the
// currently-matched backends with their latest observed active-health verdict
// (ADR 0027); a warming backend with no health record yet reports healthy null.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "id")

	row, err := h.store.LoadBalancerByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeLoadBalancerNotFound, "load balancer not found", nil)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load load balancer", nil)
		return
	}

	view := toView(row)
	view.Backends = h.buildBackends(r, row)
	response.WriteJSON(w, r, http.StatusOK, view)
}

// buildBackends resolves the load balancer's currently-matched backends and
// their latest active-health verdict. It is best-effort: a failure to list the
// owner's VMs or the health verdicts degrades to an empty (or verdict-less)
// backends array rather than failing the read, since the config view is the
// primary payload and health is advisory.
func (h *Handler) buildBackends(r *http.Request, row store.LoadBalancer) []backendView {
	ctx := r.Context()

	vms, err := h.store.ListVMsByOwner(ctx, row.OwnerID)
	if err != nil {
		h.log.WarnContext(ctx, "list owner vms for lb backends",
			"loadbalancer_id", row.ID.String(), "error", err.Error())
		return []backendView{}
	}

	health, err := h.store.ListLBBackendHealth(ctx, row.ID)
	if err != nil {
		h.log.WarnContext(ctx, "list lb backend health",
			"loadbalancer_id", row.ID.String(), "error", err.Error())
		health = nil
	}

	backends := make([]backendView, 0, len(vms))
	for _, vm := range vms {
		if !selectorMatches(row.Selector, vm.Labels) {
			continue
		}
		bv := backendView{VMID: vm.ID.String(), VMName: vm.Name}
		if rec, ok := health[vm.ID]; ok {
			healthy := rec.Healthy
			reportedAt := rec.ReportedAt.UTC().Format(time.RFC3339Nano)
			bv.Healthy, bv.ReportedAt = &healthy, &reportedAt
		}
		backends = append(backends, bv)
	}
	sort.Slice(backends, func(i, j int) bool { return backends[i].VMName < backends[j].VMName })
	return backends
}
