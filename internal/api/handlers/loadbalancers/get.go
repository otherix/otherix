// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import (
	"context"
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

	ctx := r.Context()
	view := toView(row)
	vms, err := h.store.ListVMsByOwner(ctx, row.OwnerID)
	if err != nil {
		// Best-effort: the config view is the primary payload and health is
		// advisory, so a failed VM list leaves an empty backends array and a nil
		// health summary rather than failing the read.
		h.log.WarnContext(ctx, "list owner vms for lb backends",
			"loadbalancer_id", row.ID.String(), "error", err.Error())
		view.Backends = []backendView{}
		response.WriteJSON(w, r, http.StatusOK, view)
		return
	}
	backends := h.buildBackends(ctx, row, vms)
	view.Backends = backends
	view.Health = summarizeBackends(backends)
	response.WriteJSON(w, r, http.StatusOK, view)
}

// buildBackends resolves the load balancer's currently-matched backends and
// their latest active-health verdict from the owner's VMs the caller passes in
// (already fetched, so the list path can reuse one per-owner scan across the
// page). It is best-effort: a failure to list the health verdicts degrades to
// verdict-less backends rather than failing, since health is advisory.
func (h *Handler) buildBackends(ctx context.Context, row store.LoadBalancer, vms []store.VM) []backendView {
	health, err := h.store.ListLBBackendHealth(ctx, row.ID)
	if err != nil {
		h.log.WarnContext(ctx, "list lb backend health",
			"loadbalancer_id", row.ID.String(), "error", err.Error())
		health = nil
	}

	staleness := healthStalenessWindow(row.HealthCheck, row.Port)
	now := time.Now()
	backends := make([]backendView, 0, len(vms))
	for _, vm := range vms {
		if !selectorMatches(row.Selector, vm.Labels) {
			continue
		}
		bv := backendView{VMID: vm.ID.String(), VMName: vm.Name}
		// Only a FRESH record renders a verdict; a stale one is treated as absent
		// (healthy/reported_at stay null), matching connect eligibility, which
		// applies the same window, and the spec's "no fresh record -> null" rule.
		if rec, ok := health[vm.ID]; ok && now.Sub(rec.ReportedAt) <= staleness {
			healthy := rec.Healthy
			reportedAt := rec.ReportedAt.UTC().Format(time.RFC3339Nano)
			bv.Healthy, bv.ReportedAt = &healthy, &reportedAt
		}
		backends = append(backends, bv)
	}
	sort.Slice(backends, func(i, j int) bool { return backends[i].VMName < backends[j].VMName })
	return backends
}

// summarizeBackends rolls the per-backend verdicts up into the aggregate health
// summary. total counts every selector-matched backend; healthy and unhealthy
// count only those with a FRESH verdict (Healthy non-nil) - buildBackends
// already applied the staleness window, so a stale/absent/warming record leaves
// Healthy nil and is counted as neither.
func summarizeBackends(backends []backendView) *healthSummaryView {
	total := len(backends)
	var healthy, unhealthy int
	for _, b := range backends {
		if b.Healthy == nil {
			continue
		}
		if *b.Healthy {
			healthy++
		} else {
			unhealthy++
		}
	}
	return &healthSummaryView{
		Status:         deriveLBHealthStatus(total, healthy, unhealthy),
		TargetsTotal:   total,
		TargetsHealthy: healthy,
	}
}

// deriveLBHealthStatus maps the (total, healthy, unhealthy) target counts onto
// the aggregate status label. A serving-but-not-yet-confirmed load balancer (a
// mix that includes warming backends) reads degraded, never unhealthy: only an
// all-confirmed-down set reports unhealthy.
func deriveLBHealthStatus(total, healthy, unhealthy int) string {
	switch {
	case total == 0:
		return "no_backends"
	case healthy == total:
		return "healthy"
	case unhealthy == total:
		return "unhealthy"
	default:
		return "degraded"
	}
}
