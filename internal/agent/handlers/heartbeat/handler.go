// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package heartbeat serves the agent's heartbeat control endpoints.
package heartbeat

import "net/http"

// Nudger triggers an immediate heartbeat. Implemented by *heartbeat.Sender.
type Nudger interface{ Nudge() }

// Handler serves POST /v1/heartbeat/nudge.
type Handler struct{ nudger Nudger }

// New returns a Handler that triggers n on nudge.
func New(n Nudger) *Handler { return &Handler{nudger: n} }

// Nudge triggers an immediate heartbeat and returns 204. This is the fast-push
// hot path: the CP calls it after a cutover so this node re-pulls its
// declared_fdb without waiting for the next interval tick.
func (h *Handler) Nudge(w http.ResponseWriter, r *http.Request) {
	h.nudger.Nudge()
	w.WriteHeader(http.StatusNoContent)
}
