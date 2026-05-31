// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package health implements /healthz (liveness) and /readyz (readiness)
// for the api-server. The two endpoints intentionally do not live under
// /v1/ — Kubernetes probes are version-independent infrastructure paths.
//
// /healthz is a process-liveness signal: returns 200 as long as the
// handler can be reached. It must not call out to dependencies.
// /readyz pings the database (and, in the future, any other dependency
// the api-server cannot serve traffic without) and returns 503 if any
// check fails.
package health

import (
	"context"
	"net/http"
	"time"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/version"
)

const pingTimeout = 2 * time.Second

// Wire-format status strings reported in the response bodies of /healthz
// and /readyz, and inside ReadyResponse.Checks.
const (
	statusOK       = "ok"
	statusFail     = "fail"
	statusNotReady = "not_ready"
)

// LiveResponse is the body of /healthz.
type LiveResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
}

// ReadyResponse is the body of /readyz.
type ReadyResponse struct {
	Status  string           `json:"status"`
	Version string           `json:"version"`
	Checks  map[string]Check `json:"checks"`
}

// Check is the per-dependency outcome inside ReadyResponse.Checks.
type Check struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// Pinger is the readiness dependency the api-server cannot serve traffic
// without: the storage backend. *store.Store (pgx) and *etcdstore.Store both
// satisfy it, so /readyz is backend-agnostic.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Handler holds the dependencies required to answer health probes.
type Handler struct {
	pinger    Pinger
	checkName string
}

// New returns a Handler whose readiness probe pings p, reporting the outcome
// under checkName in the /readyz response (e.g. "database" for pgx, "etcd").
func New(p Pinger, checkName string) *Handler {
	return &Handler{pinger: p, checkName: checkName}
}

// Live answers /healthz with 200 and a tiny payload identifying the
// running build. Used as the Kubernetes liveness probe.
func (h *Handler) Live(w http.ResponseWriter, r *http.Request) {
	response.WriteJSON(w, r, http.StatusOK, LiveResponse{
		Status:  statusOK,
		Version: version.Version,
	})
}

// Ready answers /readyz. Each dependency check populates the Checks map;
// any failure flips the overall status to 503 / "not_ready". Used as the
// Kubernetes readiness probe.
func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	checks := map[string]Check{}
	overall := http.StatusOK

	pingCtx, cancel := context.WithTimeout(r.Context(), pingTimeout)
	defer cancel()
	if err := h.pinger.Ping(pingCtx); err != nil {
		checks[h.checkName] = Check{Status: statusFail, Error: err.Error()}
		overall = http.StatusServiceUnavailable
	} else {
		checks[h.checkName] = Check{Status: statusOK}
	}

	status := statusOK
	if overall != http.StatusOK {
		status = statusNotReady
	}

	response.WriteJSON(w, r, overall, ReadyResponse{
		Status:  status,
		Version: version.Version,
		Checks:  checks,
	})
}
