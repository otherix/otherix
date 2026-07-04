// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
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
	if row.PublishedPort != nil {
		view.Listeners = h.buildListeners(ctx, row)
	}
	response.WriteJSON(w, r, http.StatusOK, view)
}

// listenerStalenessWindow is the fixed freshness window for an observed
// published-listener status. Unlike backend health - whose window scales with
// the effective probe interval - a listener status has no per-listener probe
// cadence: it is CP-stamped on heartbeat receipt, so it cannot refresh faster
// than the agent heartbeat. The window is the heartbeat floor times the
// staleness factor (30s x 3 = 90s); a status older than that is treated as
// absent, so a dead gateway's last-reported row does not render as a live
// listener.
const listenerStalenessWindow = time.Duration(store.HealthCheckHeartbeatFloorSeconds*store.HealthCheckStalenessFactor) * time.Second

// buildListeners resolves the observed per-gateway bind status of a published
// load balancer's public listener. It is best-effort: a failure to list the
// status degrades to no listeners rather than failing the read, since the
// config view is the primary payload. Only a FRESH status renders; a row older
// than listenerStalenessWindow is treated as absent. Callers pass only published
// load balancers (the caller gates on PublishedPort).
func (h *Handler) buildListeners(ctx context.Context, row store.LoadBalancer) []listenerStatusView {
	statuses, err := h.store.ListLBPublishedListenerStatus(ctx, row.ID)
	if err != nil {
		h.log.WarnContext(ctx, "list lb published listener status",
			"loadbalancer_id", row.ID.String(), "error", err.Error())
		return nil
	}
	published := *row.PublishedPort
	now := time.Now()
	out := make([]listenerStatusView, 0, len(statuses))
	for _, s := range statuses {
		if now.Sub(s.ReportedAt) > listenerStalenessWindow {
			continue
		}
		// Resolve the gateway to its name and advertised endpoint. On any resolve
		// error (node gone mid-window / transient) skip the entry rather than
		// emit a half-populated one or fail the read; listeners are advisory.
		node, err := h.store.NodeByID(ctx, s.NodeID)
		if err != nil {
			h.log.WarnContext(ctx, "resolve listener status node",
				"loadbalancer_id", row.ID.String(), "node_id", s.NodeID.String(), "error", err.Error())
			continue
		}
		out = append(out, listenerStatusView{
			Node:       node.Name,
			Address:    gatewayListenerAddress(node.AdvertisedEndpoint, published),
			Bound:      s.Bound,
			Error:      s.Error,
			ReportedAt: s.ReportedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// gatewayListenerAddress builds the client connect target for a published
// listener: the host of the gateway node's advertised endpoint joined with the
// published port. It strips the endpoint's scheme (the advertised endpoint is a
// URL like https://host:port) and falls back to a bare host:port or the raw
// string when the endpoint does not parse as a URL.
func gatewayListenerAddress(advertisedEndpoint string, publishedPort int32) string {
	var host string
	switch u, err := url.Parse(advertisedEndpoint); {
	case err == nil && u.Host != "":
		host = u.Hostname()
	default:
		if h, _, splitErr := net.SplitHostPort(advertisedEndpoint); splitErr == nil {
			host = h
		} else {
			host = advertisedEndpoint
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(int(publishedPort)))
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
