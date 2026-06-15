// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import "github.com/otherix/otherix/internal/store"

// User-facing VM status strings. Surfaced through the public API (the
// `status` field on VM). Distinct from store.VMDesiredPhase (user
// intent) and store.VMPhase (agent-observed runtime) — this is the
// projected union the API exposes.
const (
	statusPending   = "pending"
	statusCreating  = "creating"
	statusRunning   = "running"
	statusPaused    = "paused"
	statusStopped   = "stopped"
	statusError     = "error"
	statusGone      = "gone"
	statusOrphaned  = "orphaned"
	statusDeleting  = "deleting"
	statusMigrating = "migrating"
)

// projectStatus maps the (vms row, vm_runtime row) pair plus the
// "delete in flight" bit to the user-facing status string per the
// Phase B truth table:
//
//	deleted_at not null              → gone
//	active vm.delete task            → deleting  (teardown in flight, pre-tombstone)
//	active migration                 → migrating (move in flight, masks runtime flicker)
//	scheduling_status == unscheduled → pending   (awaiting placement)
//	runtime nil              → creating  (worker hasn't upserted yet)
//	runtime.phase == pending → creating  (agent task still in flight)
//	runtime.phase == migrating → migrating (post-cutover tail: the
//	    activeMigration overlay is off but the target still reports the
//	    incoming VM as migrating)
//	runtime.phase == running → running
//	runtime.phase == paused  → paused
//	runtime.phase == stopped → stopped
//	runtime.phase == error   → error
//	runtime.phase == gone    → gone
//	runtime.phase == orphaned → orphaned
//
// deleting sits below the deleted_at tombstone (a gone VM is gone, not
// deleting) and above everything else: a delete in flight dominates the
// observed runtime phase (which still reads running/stopped until the
// agent tears qemu down). The caller supplies deleting from a live
// vm.delete task lookup (no desired-state write — see delete.go).
//
// migrating sits just below deleting and above the unscheduled/runtime
// branches: a delete in flight (terminal user intent) dominates;
// otherwise an active migration dominates the observed runtime flicker
// (the move drives the target heartbeat through pending->running, which
// would otherwise project creating->running mid-move). A migrating VM is
// always scheduled and running, so this branch never masks a genuine
// unscheduled/creating state. The caller supplies migrating from the
// in-flight (non-terminal) migration lookup it already performs.
//
// Pure function: callers pass the vm row, a (possibly nil) runtime
// pointer, and the deleting + migrating bits. Tested independently of
// the GET handler — the truth table is the single source of behavioural
// truth for the wire `status` field.
func projectStatus(vm store.VM, runtime *store.VMRuntime, deleting, migrating bool) string {
	if vm.DeletedAt != nil {
		return statusGone
	}
	if deleting {
		return statusDeleting
	}
	if migrating {
		return statusMigrating
	}
	if vm.SchedulingStatus == store.VMSchedulingUnscheduled {
		return statusPending
	}
	if runtime == nil {
		return statusCreating
	}
	switch runtime.Phase {
	case store.VmPhasePending:
		return statusCreating
	case store.VmPhaseMigrating:
		// Post-cutover tail: cutover committed so the activeMigration
		// overlay is off (the migrating bit checked above is false), but
		// the target still reports the incoming VM as migrating. Project
		// migrating, not creating.
		return statusMigrating
	case store.VmPhaseRunning:
		return statusRunning
	case store.VmPhasePaused:
		return statusPaused
	case store.VmPhaseStopped:
		return statusStopped
	case store.VmPhaseError:
		return statusError
	case store.VmPhaseOrphaned:
		return statusOrphaned
	case store.VmPhaseGone:
		return statusGone
	}
	// Unknown phase — treat as creating (defensive; new phases must
	// land here as the schema enum grows).
	return statusCreating
}
