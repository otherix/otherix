// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import "github.com/otherix/otherix/internal/store"

// User-facing VM status strings. Surfaced through the public API (the
// `status` field on VM). Distinct from store.VMDesiredPhase (user
// intent) and store.VMPhase (agent-observed runtime) — this is the
// projected union the API exposes.
const (
	statusPending  = "pending"
	statusCreating = "creating"
	statusRunning  = "running"
	statusPaused   = "paused"
	statusStopped  = "stopped"
	statusError    = "error"
	statusGone     = "gone"
	statusOrphaned = "orphaned"
)

// projectStatus maps the (vms row, vm_runtime row) pair to the
// user-facing status string per the Phase B truth table:
//
//	deleted_at not null              → gone
//	scheduling_status == unscheduled → pending  (awaiting placement)
//	runtime nil              → creating  (worker hasn't upserted yet)
//	runtime.phase == pending → creating  (agent task still in flight)
//	runtime.phase == running → running
//	runtime.phase == paused  → paused
//	runtime.phase == stopped → stopped
//	runtime.phase == error   → error
//	runtime.phase == gone    → gone
//	runtime.phase == orphaned → orphaned
//
// Pure function: callers pass the vm row plus a (possibly nil) runtime
// pointer. Tested independently of the GET handler — the truth table
// is the single source of behavioural truth for the wire `status`
// field.
func projectStatus(vm store.VM, runtime *store.VMRuntime) string {
	if vm.DeletedAt != nil {
		return statusGone
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
