// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package loadbalancers

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/api/handlers/gateways"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// lbConnectResponse extends the VM ingress response with the chosen backend's
// name, which the relay transport needs to address /v1/vms/{name}/relay.
type lbConnectResponse struct {
	Transport         string `json:"transport"`
	VMID              string `json:"vm_id"`
	VMName            string `json:"vm_name"`
	Port              int    `json:"port"`
	SplicerAddr       string `json:"splicer_addr,omitempty"`
	SplicerServerName string `json:"splicer_server_name,omitempty"`
	SessionCred       string `json:"session_cred,omitempty"`
	ExpiresAt         string `json:"expires_at,omitempty"`
}

// Connect implements POST /v1/loadbalancers/{id}/connect, where {id} is the
// load-balancer NAME: it selects a CP-eligible backend VM from the load
// balancer's label-selected pool and returns the ingress connect coordinates
// for it. Backend selection is broker-time and stateless; the client re-brokers
// per connection, so each connection is balanced independently. It is mounted
// OUTSIDE the Idempotency middleware so a replayed Idempotency-Key never returns
// a cached, expiring session credential and freezes balancing.
func (h *Handler) Connect(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "id")
	user := auth.UserFromContext(r.Context())
	if user == nil {
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "authentication required", nil)
		return
	}

	lb, err := h.store.LoadBalancerByName(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeNotFound(w, r)
			return
		}
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "load load balancer", nil)
		return
	}

	if err := auth.CheckOwnership(user, &lb.OwnerID, auth.PermLoadBalancerConnect); err != nil {
		// Cross-owner connect is 404 (never 403), mirroring the VM broker: the
		// connect leg never confirms a foreign load balancer exists.
		h.writeNotFound(w, r)
		return
	}

	candidates, err := h.eligibleBackends(r.Context(), lb)
	if err != nil {
		h.log.ErrorContext(r.Context(), "loadbalancers.connect resolve backends",
			"lb", lb.ID, "error", err.Error())
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "resolve backends", nil)
		return
	}
	if len(candidates) == 0 {
		response.WriteError(w, r, http.StatusConflict,
			response.CodeIngressUnavailable, "no eligible backend for load balancer", nil)
		return
	}

	// Random order, then take the first backend the broker can actually resolve
	// (a backend can pass CP-eligibility yet momentarily lack a converged
	// gateway). At most one session credential is minted (for the chosen one).
	shuffleVMs(candidates)
	var hardErr error
	for _, vm := range candidates {
		res, rerr := h.broker.ResolveIngress(r.Context(), vm, int(lb.Port))
		if rerr != nil {
			if errors.Is(rerr, gateways.ErrIngressUnavailable) {
				continue
			}
			// A hard resolve error for THIS backend (e.g. a transient store/gateway
			// read). Log it and try the remaining candidates rather than failing the
			// whole connect on one bad backend; remember it so an all-hard-error
			// outcome still surfaces a 500 instead of a benign "no reachable backend".
			h.log.ErrorContext(r.Context(), "loadbalancers.connect resolve ingress",
				"lb", lb.ID, "vm", vm.ID, "error", rerr.Error())
			hardErr = rerr
			continue
		}
		resp := lbConnectResponse{
			Transport: res.Transport,
			VMID:      res.VMID.String(),
			VMName:    res.VMName,
			Port:      res.Port,
		}
		if res.Transport == "gateway" {
			resp.SplicerAddr = res.SplicerAddr
			resp.SplicerServerName = res.SplicerServerName
			resp.SessionCred = res.SessionCred
			resp.ExpiresAt = res.ExpiresAt.UTC().Format(time.RFC3339)
		}
		response.WriteJSON(w, r, http.StatusOK, resp)
		return
	}

	// Every eligible backend failed to resolve. If at least one failed with a hard
	// error (not the benign not-yet-converged signal), surface it as internal
	// rather than masking a systemic resolve failure as a benign unavailability.
	if hardErr != nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "resolve ingress", nil)
		return
	}
	// Every eligible backend was merely unconverged (no converged gateway yet).
	response.WriteError(w, r, http.StatusConflict,
		response.CodeIngressUnavailable, "no reachable backend for load balancer", nil)
}

// eligibleBackends returns the LB owner's VMs that match the selector AND are
// observed running, then subtracts any backend a fresh active-health record
// confirms is not healthy. A VM whose runtime is absent or not running is
// excluded (fail toward not handing out a backend we cannot confirm is up). The
// health filter is subtractive and fails toward inclusion: a backend is dropped
// only on a record that EXISTS, reports Healthy==false, AND is fresh (reported
// within HealthCheckStalenessFactor x the effective probe interval). An absent
// or stale record - or a failed health read - leaves the running backend in, so
// a broken probe pipeline never darkens a healthy load balancer (ADR 0027).
func (h *Handler) eligibleBackends(ctx context.Context, lb store.LoadBalancer) ([]store.VM, error) {
	vms, err := h.store.ListVMsByOwner(ctx, lb.OwnerID)
	if err != nil {
		return nil, err
	}
	health, err := h.store.ListLBBackendHealth(ctx, lb.ID)
	if err != nil {
		// Health is advisory; a read failure must not dark the LB. Degrade to the
		// phase==running set (fail toward inclusion).
		h.log.WarnContext(ctx, "loadbalancers.connect health read failed; degrading to running-only",
			"lb", lb.ID, "error", err.Error())
		health = nil
	}
	staleness := healthStalenessWindow(lb.HealthCheck, lb.Port)
	now := time.Now()
	out := make([]store.VM, 0, len(vms))
	for _, vm := range vms {
		if !selectorMatches(lb.Selector, vm.Labels) {
			continue
		}
		rt, err := h.store.VMRuntimeByID(ctx, vm.ID)
		if err != nil {
			// Absent runtime (ErrNotFound) is a clean exclusion. A non-NotFound
			// (transient) error is also excluded (fail toward not handing out an
			// unconfirmed backend), but log it so an infra fault is not fully
			// hidden behind the eventual 409.
			if !errors.Is(err, store.ErrNotFound) {
				h.log.WarnContext(ctx, "loadbalancers.connect runtime read failed",
					"lb", lb.ID, "vm", vm.ID, "error", err.Error())
			}
			continue
		}
		if rt.Phase != store.VmPhaseRunning {
			continue // not running -> excluded
		}
		if rec, ok := health[vm.ID]; ok && !rec.Healthy && now.Sub(rec.ReportedAt) <= staleness {
			continue // fresh, confirmed-not-healthy -> exclude (warming and down both land here)
		}
		out = append(out, vm)
	}
	return out, nil
}

// healthStalenessWindow is the freshness window a stored active-health verdict
// must fall within to be trusted. It is shared by connect eligibility and the
// get view so both judge freshness identically. The window is
// HealthCheckStalenessFactor x the effective probe interval, floored at
// HealthCheckHeartbeatFloorSeconds: the observed verdict is CP-stamped on
// heartbeat receipt and cannot advance faster than the agent heartbeat cadence,
// so a window derived from a short probe interval alone would judge a
// confirmed-unhealthy record stale in the gap between heartbeats (defeating the
// exclude). Flooring at a few heartbeats keeps a short-interval verdict fresh.
func healthStalenessWindow(hc store.LoadBalancerHealthCheck, lbPort int32) time.Duration {
	secs := max(hc.EffectiveFor(lbPort).IntervalSeconds, store.HealthCheckHeartbeatFloorSeconds)
	return time.Duration(secs) * time.Second * store.HealthCheckStalenessFactor
}

// shuffleVMs randomizes the candidate order in place for stateless balancing.
func shuffleVMs(vms []store.VM) {
	rand.Shuffle(len(vms), func(i, j int) { vms[i], vms[j] = vms[j], vms[i] })
}
