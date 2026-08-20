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

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/vm"
)

// VMManager is the narrow vm.Manager surface the VM reconciler needs.
// vm.Manager satisfies it structurally; tests pass a fake without
// importing the production package. The manager exposes
// HasInFlight so the reconciler can short-circuit corrective ops
// while a prior tick's enqueue is still running.
type VMManager interface {
	List() []*vm.VM
	HasInFlight(name string) bool
	GuestMemUsedMiB(name string) *int64
	Start(ctx context.Context, name string) (*vm.AgentTask, error)
	Stop(ctx context.Context, name string) (*vm.AgentTask, error)
	DeleteByName(ctx context.Context, name string) (*vm.AgentTask, error)
	// Delete tears a VM down by UUID. UUID-keyed by requirement: a tombstone
	// names a VM whose CP-side name guard is already released, so the name may
	// belong to a different VM by now.
	Delete(ctx context.Context, vmID uuid.UUID) (*vm.AgentTask, error)
	// HasActiveMigration reports whether a non-terminal migration names this
	// VM, in either role. Teardown must not race the migration state machine.
	HasActiveMigration(vmID uuid.UUID) bool
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

	desired    atomic.Pointer[[]heartbeat.DeclaredVM]
	tombstones atomic.Pointer[[]heartbeat.VMTombstone]
	trigger    chan struct{}

	mu      sync.Mutex
	reports map[string]heartbeat.VMReport

	teardownMu      sync.Mutex
	lastTeardownTry map[uuid.UUID]time.Time
}

// teardownRetryInterval is the minimum spacing between teardown attempts for
// one tombstoned VM. Without it a teardown that fails leaves the VM at
// StatusFailed with its in-flight slot released, so every tick would re-enter
// Manager.Delete - a permanent SIGKILL loop plus a meta.json rewrite per tick
// on a genuinely stuck pid. It must never become "never retry": the CP
// re-sends the tombstone every heartbeat precisely so a transient failure
// self-heals once the stuck pid is reaped.
const teardownRetryInterval = time.Minute

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
		log:             log,
		manager:         manager,
		tick:            tick,
		trigger:         make(chan struct{}, 1),
		reports:         map[string]heartbeat.VMReport{},
		lastTeardownTry: map[uuid.UUID]time.Time{},
	}, nil
}

// HandleHeartbeatResponse implements heartbeat.ResponseHandler. Copies
// the declared_vms and vm_tombstones slices (the sender's response
// struct may be reused) and nudges the reconciler. Nil response is a
// no-op.
func (r *VMs) HandleHeartbeatResponse(_ context.Context, resp *heartbeat.Response) {
	if resp == nil {
		return
	}
	vms := append([]heartbeat.DeclaredVM(nil), resp.DeclaredVMs...)
	r.desired.Store(&vms)
	ts := append([]heartbeat.VMTombstone(nil), resp.VMTombstones...)
	r.tombstones.Store(&ts)
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

// reconcile is one pass. It first tears down every VM the CP has
// tombstoned (see reconcileTombstones), then walks the (desired,
// observed) diff: builds the reports map from Manager.List() and
// dispatches corrective lifecycle ops:
//
//   - desired_phase=running, observed=stopped → Start
//   - desired_phase=stopped, observed=running → Stop (graceful)
//   - desired_phase=deleted, observed≠deleting → DeleteByName
//   - any in-flight (HasInFlight==true)        → skip enqueue
//   - observed=failed                          → skip
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
	observed := r.manager.List()
	// Tombstones first: the reports loop below does a QMP round-trip per
	// running VM, and a destructive correction must not queue behind an
	// unrelated stats read on a hung socket.
	r.reconcileTombstones(ctx, observed)

	desiredPtr := r.desired.Load()
	var desired []heartbeat.DeclaredVM
	if desiredPtr != nil {
		desired = *desiredPtr
	}
	desiredByName := make(map[string]heartbeat.DeclaredVM, len(desired))
	for _, d := range desired {
		desiredByName[d.Name] = d
	}

	nextReports := make(map[string]heartbeat.VMReport, len(observed))
	for _, v := range observed {
		var memUsed *int64
		if v.Status == vm.StatusRunning {
			memUsed = r.manager.GuestMemUsedMiB(v.Name)
		}
		nextReports[v.ID.String()] = vmReport(v, memUsed)
	}

	for _, v := range observed {
		decl, declared := desiredByName[v.Name]
		if !declared {
			// VM observed locally but CP does not declare it on this
			// node. Could be: 1) CP-side delete tasks lost; 2) VM
			// migrated away and CP forgot to declare elsewhere.
			// CP orphan-detects via omission; agent simply
			// reports the VM in heartbeat and waits. No corrective op.
			continue
		}
		r.dispatch(ctx, v, decl)
	}

	r.mu.Lock()
	r.reports = nextReports
	r.mu.Unlock()
}

// reconcileTombstones tears down every VM the control plane has tombstoned.
//
// A tombstone is the ONLY teardown trigger: the reconciler never destroys a VM
// because the CP stopped declaring it (declared_vms is fail-open and is nil on
// every agent boot before the first response lands).
//
// Teardown is asynchronous and is never awaited. While this agent still holds
// the VM it keeps reporting it, so the CP keeps re-sending the tombstone, and
// the signal stops the tick after the VM is gone.
func (r *VMs) reconcileTombstones(ctx context.Context, observed []*vm.VM) {
	tsPtr := r.tombstones.Load()
	if tsPtr == nil {
		return
	}
	r.pruneTeardownAttempts(*tsPtr)

	// List returns copies, so these are snapshots: read ID / Name / Status
	// only, never mutate through them.
	byID := make(map[uuid.UUID]*vm.VM, len(observed))
	for _, v := range observed {
		byID[v.ID] = v
	}
	for _, ts := range *tsPtr {
		v, known := byID[ts.VMID]
		if !known {
			// Nothing to tear down. The CP stops sending the tombstone once
			// this node stops reporting the VM.
			continue
		}
		if r.manager.HasActiveMigration(ts.VMID) {
			r.log.InfoContext(ctx, "tombstoned vm has an active migration; deferring teardown",
				slog.String("vm_id", ts.VMID.String()), slog.String("vm_name", ts.VMName))
			continue
		}
		if !teardownAllowedFor(v.Status) {
			r.log.InfoContext(ctx, "tombstoned vm is not in a tearable state; deferring teardown",
				slog.String("vm_id", ts.VMID.String()), slog.String("vm_name", ts.VMName),
				slog.String("status", string(v.Status)))
			continue
		}
		if r.manager.HasInFlight(v.Name) {
			continue
		}
		if !r.markTeardownAttempt(ts.VMID) {
			continue
		}
		if _, err := r.manager.Delete(ctx, ts.VMID); err != nil {
			r.log.WarnContext(ctx, "tombstoned vm teardown failed; will retry",
				slog.String("vm_id", ts.VMID.String()), slog.String("vm_name", ts.VMName),
				slog.String("err", err.Error()))
			continue
		}
		r.log.InfoContext(ctx, "tearing down tombstoned vm",
			slog.String("vm_id", ts.VMID.String()), slog.String("vm_name", ts.VMName))
	}
}

// teardownAllowedFor enumerates every vm.Status (spec.go) against the teardown
// decision, deliberately instead of a coarse "transitional" predicate:
//
//   - migrating_incoming: NO. The target holds an incoming guest whose dest
//     disk may be the only copy; killing it is irreversible.
//   - deleting: YES. That is a teardown this agent crashed part-way through -
//     the exact wedge a tombstone exists to re-drive. Manager.Delete is
//     idempotent and self-heals a stuck pid.
//   - failed: YES. A failed VM still owns a qemu process and disk images.
//   - pending / creating / running / paused / stopping / stopped: YES, the
//     ordinary path.
//
// A status added later lands in the default arm and is NOT torn down: an
// irreversible action fails toward inaction until this switch is revisited.
func teardownAllowedFor(s vm.Status) bool {
	switch s {
	case vm.StatusPending, vm.StatusCreating, vm.StatusRunning, vm.StatusPaused,
		vm.StatusStopping, vm.StatusStopped, vm.StatusFailed, vm.StatusDeleting:
		return true
	case vm.StatusMigratingIncoming:
		return false
	default:
		return false
	}
}

// markTeardownAttempt records a teardown attempt for vmID and reports whether
// the caller may proceed. False means the previous attempt is still inside
// teardownRetryInterval.
func (r *VMs) markTeardownAttempt(vmID uuid.UUID) bool {
	r.teardownMu.Lock()
	defer r.teardownMu.Unlock()
	now := time.Now()
	if last, ok := r.lastTeardownTry[vmID]; ok && now.Sub(last) < teardownRetryInterval {
		return false
	}
	r.lastTeardownTry[vmID] = now
	return true
}

// pruneTeardownAttempts drops attempt records for VMs the current tombstone
// list no longer names, so the map cannot grow without bound.
func (r *VMs) pruneTeardownAttempts(tombstones []heartbeat.VMTombstone) {
	r.teardownMu.Lock()
	defer r.teardownMu.Unlock()
	if len(r.lastTeardownTry) == 0 {
		return
	}
	live := make(map[uuid.UUID]struct{}, len(tombstones))
	for _, ts := range tombstones {
		live[ts.VMID] = struct{}{}
	}
	for id := range r.lastTeardownTry {
		if _, ok := live[id]; !ok {
			delete(r.lastTeardownTry, id)
		}
	}
}

// dispatch handles one observed-vs-declared pair. Side-effect: may
// enqueue a Manager lifecycle op when convergence requires action.
func (r *VMs) dispatch(ctx context.Context, v *vm.VM, decl heartbeat.DeclaredVM) {
	if r.manager.HasInFlight(v.Name) {
		return
	}
	if v.Status == vm.StatusFailed {
		// Failed VMs require operator intervention.
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

// vmReport projects a vm.VM snapshot to heartbeat.VMReport. Surfaces
// vm_uuid + phase, plus memory_used_mib (nil for non-running VMs or when
// the balloon stats read failed); pid / observed_generation / timestamps
// are forward-compatibility slots that the agent does not yet
// populate (vm.VM has no field for observed_generation; CP records
// it CP-side via the desired generation in declared_vms once the
// reconciler runs).
func vmReport(v *vm.VM, memUsedMiB *int64) heartbeat.VMReport {
	return heartbeat.VMReport{
		VMUUID:        v.ID,
		Phase:         mapPhase(v.Status),
		MemoryUsedMib: memUsedMiB,
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
