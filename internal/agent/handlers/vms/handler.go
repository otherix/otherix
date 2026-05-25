// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package vms hosts the agent's /v1/vms/* HTTP handlers — the surface
// the control plane (and curl-driven smoke tests) uses to drive VM
// lifecycle on this node. POST /v1/vms and DELETE /v1/vms/{vm_name}
// are async: they return 202 with a task_id that the caller polls
// through GET /v1/tasks/{task_id} (handled by the sibling tasks
// package). The per-VM routes are keyed by name; the VM's UUID still
// surfaces in the response body.
package vms

import (
	"log/slog"

	"github.com/go-chi/chi/v5"

	"github.com/otherix/otherix/internal/agent/console"
	"github.com/otherix/otherix/internal/agent/vm"
)

// Handler bundles the VM manager dependency and the logger. The
// manager owns all state - including the per-VM serial multiplexer
// registry (see ADR 0029) - so the handler only translates wire
// shapes. The TokenStore is injected so tests can swap a
// clock-controlled instance without touching production wiring.
type Handler struct {
	manager *vm.Manager
	tokens  *console.TokenStore
	log     *slog.Logger
}

// New constructs a Handler. Production callers use the package's
// zero-config NewTokenStore. The per-VM console single-attach
// invariant (former internal/agent/console.ConnectionTracker) is now
// enforced inside serialmux.Multiplexer.SubscribeConsole, so the
// Handler no longer takes a ConnectionTracker.
func New(m *vm.Manager, tokens *console.TokenStore, log *slog.Logger) *Handler {
	return &Handler{manager: m, tokens: tokens, log: log}
}

// Mount registers /v1/vms routes on r. The console-stream route is
// intentionally NOT registered here — it's long-lived and mounts in
// internal/agent/server.go::buildRouter outside the middleware.Timeout
// Group. Adding it back here would re-introduce the ~30s session-drop
// bug closed by the timeout-fix iteration.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/", h.List)
	r.Post("/", h.Create)
	r.Get("/{vm_name}", h.Get)
	r.Delete("/{vm_name}", h.Delete)
	r.Post("/{vm_name}/pause", h.Pause)
	r.Post("/{vm_name}/resume", h.Resume)
	r.Post("/{vm_name}/reset", h.Reset)
	r.Post("/{vm_name}/start", h.Start)
	r.Post("/{vm_name}/stop", h.Stop)
	r.Post("/{vm_name}/poweroff", h.Poweroff)
	r.Post("/{vm_name}/reboot", h.Reboot)
	r.Post("/{vm_name}/console-token", h.ConsoleIssueToken)
}

// vmView is the wire shape returned for a single VM. The wire field
// is `pool` (was `pool_id`) for consistency with the CP edge; the
// value is the agent-local pool name string (the agent's pool
// registry is name-keyed; the per-instance UUID stays at the
// operator-API edge).
type vmView struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	VCPUs        int    `json:"vcpus"`
	MemoryMB     int    `json:"memory_mb"`
	Pool         string `json:"pool"`
	Architecture string `json:"architecture"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

func toView(v *vm.VM) vmView {
	return vmView{
		ID:           v.ID.String(),
		Name:         v.Name,
		VCPUs:        v.VCPUs,
		MemoryMB:     v.MemoryMB,
		Pool:         v.PoolName,
		Architecture: string(v.Architecture),
		Status:       string(v.Status),
		CreatedAt:    v.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:    v.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}
