// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/vm"
)

// VMManager is the narrow vm.Manager surface the VM reconciler needs.
// vm.Manager satisfies it structurally; tests pass a fake without
// importing the production package. Per L3 D2 the manager exposes
// HasInFlight so the reconciler can short-circuit corrective ops
// while a prior tick's enqueue is still running.
type VMManager interface {
	List() []*vm.VM
	HasInFlight(name string) bool
	Start(ctx context.Context, name string) (*vm.AgentTask, error)
	Stop(ctx context.Context, name string) (*vm.AgentTask, error)
	DeleteByName(ctx context.Context, name string) (*vm.AgentTask, error)
}

// VMs is the per-resource reconciler for VMs. Single instance per
// agent process; owned by the agent's server-level glue. Mirrors the
// storage-pool reconciler shape (atomic.Pointer cache + buffered
// trigger channel + lock-protected reports map) so wiring stays
// uniform across resource types.
//
// Implements heartbeat.ResponseHandler (HandleHeartbeatResponse) +
// heartbeat.VMReporter (VMReports). The sender wires the same
// reconciler to both seams.
type VMs struct {
	log     *slog.Logger
	manager VMManager
	tick    time.Duration

	desired atomic.Pointer[[]heartbeat.DeclaredVM]
	trigger chan struct{}

	mu      sync.Mutex
	reports map[string]heartbeat.VMReport
}

// ErrNilVMManager guards nil-injection at construction time.
var ErrNilVMManager = errors.New("reconciler: VMManager is required")

// NewVMs constructs the VM reconciler. tick==0 falls back to
// DefaultTickInterval. Returns ErrNilVMManager when manager is nil.
func NewVMs(manager VMManager, log *slog.Logger, tick time.Duration) (*VMs, error) {
	if manager == nil {
		return nil, ErrNilVMManager
	}
	if tick <= 0 {
		tick = DefaultTickInterval
	}
	return &VMs{
		log:     log,
		manager: manager,
		tick:    tick,
		trigger: make(chan struct{}, 1),
		reports: map[string]heartbeat.VMReport{},
	}, nil
}

// HandleHeartbeatResponse implements heartbeat.ResponseHandler. Copies
// the declared_vms slice (the sender's response struct may be
// reused) and nudges the reconciler. Nil response is a no-op.
func (r *VMs) HandleHeartbeatResponse(_ context.Context, resp *heartbeat.Response) {
	if resp == nil {
		return
	}
	vms := append([]heartbeat.DeclaredVM(nil), resp.DeclaredVMs...)
	r.desired.Store(&vms)
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// VMReports implements heartbeat.VMReporter. Returns the reconciler's
// observed-state cache in name-sorted order. The cache is refreshed
// at the end of every reconcile pass. Returns nil on empty so the
// JSON marshaller emits an empty array (matches HeartbeatRequest.vms
// being a required field).
func (r *VMs) VMReports() []heartbeat.VMReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reports) == 0 {
		return nil
	}
	out := make([]heartbeat.VMReport, 0, len(r.reports))
	for _, rep := range r.reports {
		out = append(out, rep)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].VMUUID.String() < out[j].VMUUID.String()
	})
	return out
}

// Run blocks until ctx is cancelled. Ticks every r.tick OR on
// trigger; each tick runs one reconcile pass.
func (r *VMs) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.tick)
	defer ticker.Stop()
	// Initial pass — sets baseline reports immediately so the first
	// heartbeat-after-boot carries a populated vms[] regardless of
	// when the first declared_vms payload lands.
	r.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			r.reconcile(ctx)
		case <-r.trigger:
			r.reconcile(ctx)
		}
	}
}

// reconcile is one pass over the (desired, observed) diff. Builds the
// reports map from Manager.List() and dispatches corrective lifecycle
// ops:
//
//   - desired_phase=running, observed=stopped → Start
//   - desired_phase=stopped, observed=running → Stop (graceful)
//   - desired_phase=deleted, observed≠deleting → DeleteByName
//   - any in-flight (HasInFlight==true)        → skip enqueue
//   - observed=failed                          → skip per Area 4-IV
//     (manual intervention)
//   - observed transitional (pending /
//     creating / stopping / deleting / paused) → skip; next tick re-checks
//
// All dispatch errors are logged but not propagated — the reconciler
// is retry-forever and eventually consistent.
func (r *VMs) reconcile(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	desiredPtr := r.desired.Load()
	var desired []heartbeat.DeclaredVM
	if desiredPtr != nil {
		desired = *desiredPtr
	}
	desiredByName := make(map[string]heartbeat.DeclaredVM, len(desired))
	for _, d := range desired {
		desiredByName[d.Name] = d
	}

	observed := r.manager.List()
	nextReports := make(map[string]heartbeat.VMReport, len(observed))
	for _, v := range observed {
		nextReports[v.ID.String()] = vmReport(v)
	}

	for _, v := range observed {
		decl, declared := desiredByName[v.Name]
		if !declared {
			// VM observed locally but CP does not declare it on this
			// node. Could be: 1) CP-side delete tasks lost; 2) VM
			// migrated away and CP forgot to declare elsewhere. Per L3
			// D4 lock, CP orphan-detects via omission; agent simply
			// reports the VM in heartbeat and waits. No corrective op.
			continue
		}
		r.dispatch(ctx, v, decl)
	}

	r.mu.Lock()
	r.reports = nextReports
	r.mu.Unlock()
}

// dispatch handles one observed-vs-declared pair. Side-effect: may
// enqueue a Manager lifecycle op when convergence requires action.
func (r *VMs) dispatch(ctx context.Context, v *vm.VM, decl heartbeat.DeclaredVM) {
	if r.manager.HasInFlight(v.Name) {
		return
	}
	if v.Status == vm.StatusFailed {
		// Area 4-IV: failed VMs require operator intervention.
		// Reconciler does NOT auto-restart even when desired=running.
		return
	}

	switch decl.DesiredPhase {
	case "running":
		switch v.Status {
		case vm.StatusStopped:
			r.log.InfoContext(ctx, "vm reconcile: starting",
				slog.String("vm", v.Name), slog.Int64("generation", decl.Generation))
			if _, err := r.manager.Start(ctx, v.Name); err != nil {
				r.log.WarnContext(ctx, "vm reconcile: start dispatch failed",
					slog.String("vm", v.Name), slog.String("err", err.Error()))
			}
		case vm.StatusRunning:
			// Converged — no action.
		default:
			// pending / creating / paused / stopping / deleting —
			// transitional, let the next tick re-check.
		}
	case "stopped":
		switch v.Status {
		case vm.StatusRunning:
			r.log.InfoContext(ctx, "vm reconcile: stopping",
				slog.String("vm", v.Name), slog.Int64("generation", decl.Generation))
			if _, err := r.manager.Stop(ctx, v.Name); err != nil {
				r.log.WarnContext(ctx, "vm reconcile: stop dispatch failed",
					slog.String("vm", v.Name), slog.String("err", err.Error()))
			}
		case vm.StatusStopped:
			// Converged — no action.
		default:
			// transitional — skip; next tick re-checks.
		}
	case "deleted":
		if v.Status == vm.StatusDeleting {
			return
		}
		r.log.InfoContext(ctx, "vm reconcile: deleting",
			slog.String("vm", v.Name), slog.Int64("generation", decl.Generation))
		if _, err := r.manager.DeleteByName(ctx, v.Name); err != nil {
			r.log.WarnContext(ctx, "vm reconcile: delete dispatch failed",
				slog.String("vm", v.Name), slog.String("err", err.Error()))
		}
	default:
		r.log.WarnContext(ctx, "vm reconcile: unknown desired_phase",
			slog.String("vm", v.Name), slog.String("desired_phase", decl.DesiredPhase))
	}
}

// vmReport projects a vm.VM snapshot to heartbeat.VMReport. Currently
// surfaces vm_uuid + phase only; pid / observed_generation / timestamps
// are forward-compatibility slots that the agent does not yet
// populate (vm.VM has no field for observed_generation; CP records
// it CP-side via the desired generation in declared_vms once the
// reconciler runs).
func vmReport(v *vm.VM) heartbeat.VMReport {
	return heartbeat.VMReport{
		VMUUID: v.ID,
		Phase:  mapPhase(v.Status),
	}
}

// mapPhase coerces an agent-side vm.Status string to the wire enum
// values declared in HeartbeatVMReport.phase (pending, running,
// migrating, paused, stopped, error, gone). Internal-only phases
// (creating, stopping, deleting) collapse to the closest user-visible
// phase to keep the wire enum stable. StatusMigratingIncoming maps to
// migrating so the CP projection shows migrating (not creating) during
// the post-cutover tail while the target still holds the incoming VM.
func mapPhase(s vm.Status) string {
	switch s {
	case vm.StatusPending, vm.StatusCreating:
		return "pending"
	case vm.StatusRunning:
		return "running"
	case vm.StatusPaused:
		return "paused"
	case vm.StatusStopping, vm.StatusStopped, vm.StatusDeleting:
		return "stopped"
	case vm.StatusMigratingIncoming:
		return "migrating"
	case vm.StatusFailed:
		return "error"
	default:
		return "error"
	}
}
