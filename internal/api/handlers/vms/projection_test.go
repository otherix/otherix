// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// TestProjectStatus locks the F6 truth table — the user-facing
// `status` string projected from (vms row, vm_runtime row).
func TestProjectStatus(t *testing.T) {
	t.Parallel()

	deleted := time.Now().UTC()
	cases := []struct {
		name     string
		vm       store.VM
		runtime  *store.VMRuntime
		deleting bool
		want     string
	}{
		{
			name: "deleted vm overrides runtime",
			vm:   store.VM{ID: uuid.New(), DeletedAt: &deleted, DesiredPhase: store.VmDesiredPhaseRunning},
			runtime: &store.VMRuntime{
				VmID:  uuid.New(),
				Phase: store.VmPhaseRunning,
			},
			want: statusGone,
		},
		{
			name:     "active delete task overrides running runtime",
			vm:       store.VM{ID: uuid.New(), SchedulingStatus: store.VMSchedulingScheduled},
			runtime:  &store.VMRuntime{Phase: store.VmPhaseRunning},
			deleting: true,
			want:     statusDeleting,
		},
		{
			name:     "deleted_at wins over an active delete task",
			vm:       store.VM{ID: uuid.New(), DeletedAt: &deleted},
			runtime:  &store.VMRuntime{Phase: store.VmPhaseStopped},
			deleting: true,
			want:     statusGone,
		},
		{
			name:    "nil runtime → creating",
			vm:      store.VM{ID: uuid.New(), DesiredPhase: store.VmDesiredPhaseRunning},
			runtime: nil,
			want:    statusCreating,
		},
		{
			name:    "runtime pending → creating",
			vm:      store.VM{ID: uuid.New()},
			runtime: &store.VMRuntime{Phase: store.VmPhasePending},
			want:    statusCreating,
		},
		{
			name:    "runtime running → running",
			vm:      store.VM{ID: uuid.New()},
			runtime: &store.VMRuntime{Phase: store.VmPhaseRunning},
			want:    statusRunning,
		},
		{
			name:    "runtime paused → paused",
			vm:      store.VM{ID: uuid.New()},
			runtime: &store.VMRuntime{Phase: store.VmPhasePaused},
			want:    statusPaused,
		},
		{
			name:    "runtime stopped → stopped",
			vm:      store.VM{ID: uuid.New()},
			runtime: &store.VMRuntime{Phase: store.VmPhaseStopped},
			want:    statusStopped,
		},
		{
			name:    "runtime error → error",
			vm:      store.VM{ID: uuid.New()},
			runtime: &store.VMRuntime{Phase: store.VmPhaseError},
			want:    statusError,
		},
		{
			name:    "runtime gone → gone",
			vm:      store.VM{ID: uuid.New()},
			runtime: &store.VMRuntime{Phase: store.VmPhaseGone},
			want:    statusGone,
		},
		{
			name:    "runtime orphaned → orphaned",
			vm:      store.VM{ID: uuid.New()},
			runtime: &store.VMRuntime{Phase: store.VmPhaseOrphaned},
			want:    statusOrphaned,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := projectStatus(tc.vm, tc.runtime, tc.deleting)
			if got != tc.want {
				t.Errorf("projectStatus(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestProjectStatus_Pending locks the pending precedence: an
// unscheduled VM projects "pending" regardless of its (absent) runtime,
// while a scheduled VM with no runtime yet still projects "creating".
func TestProjectStatus_Pending(t *testing.T) {
	t.Parallel()

	reason := store.SchedReasonPoolNotReady
	vm := store.VM{SchedulingStatus: store.VMSchedulingUnscheduled, SchedulingReason: &reason}
	if got := projectStatus(vm, nil, false); got != statusPending {
		t.Errorf("projectStatus(unscheduled) = %q, want %q", got, statusPending)
	}

	// Scheduled but no runtime yet -> creating (agent task running).
	vm2 := store.VM{SchedulingStatus: store.VMSchedulingScheduled}
	if got := projectStatus(vm2, nil, false); got != statusCreating {
		t.Errorf("projectStatus(scheduled,no-runtime) = %q, want %q", got, statusCreating)
	}
}

// TestToView_PendingSurfacesReasonAndSpec asserts that an unscheduled
// VM renders status.phase=pending plus the scheduling reason, and that
// pool / networks fall back to the SchedulingSpec when no disk / NIC
// rows exist yet (the pre-bind shape).
func TestToView_PendingSurfacesReasonAndSpec(t *testing.T) {
	t.Parallel()

	reason := store.SchedReasonNetworkNotReady
	msg := `network "lan" not ready`
	spec, err := store.MarshalSchedulingSpec(store.SchedulingSpec{PoolName: "default", NetworkName: ptr("lan")})
	if err != nil {
		t.Fatalf("MarshalSchedulingSpec: %v", err)
	}
	vm := store.VM{
		ID: uuid.New(), Name: "p", OwnerID: uuid.New(),
		SchedulingStatus: store.VMSchedulingUnscheduled, SchedulingReason: &reason,
		SchedulingMessage: &msg, SchedulingSpec: spec, Labels: []byte(`{}`),
		ImageFormat: store.ImageFormatQcow2, Architecture: store.CpuArchAmd64,
	}
	v := toView(vm, nil, vmViewNames{}, false /* deleting */, true /* includeSecrets */)
	if v.Status.Phase != statusPending || v.Status.Reason != reason {
		t.Errorf("status = %+v, want phase=pending reason=%q", v.Status, reason)
	}
	if v.Status.Message != msg {
		t.Errorf("status.message = %q, want %q", v.Status.Message, msg)
	}
	if v.Pool != "default" {
		t.Errorf("pool = %q, want default (from spec)", v.Pool)
	}
	if len(v.Networks) != 1 || v.Networks[0] != "lan" {
		t.Errorf("networks = %v, want [lan]", v.Networks)
	}
}

// ptr returns a pointer to v - a tiny test helper for building optional
// fields inline.
func ptr[T any](v T) *T { return &v }

func TestMachineTypeFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		arch store.CPUArch
		want string
	}{
		{arch: store.CpuArchAmd64, want: "q35"},
		{arch: store.CpuArchArm64, want: "virt"},
		{arch: store.CPUArch("unknown"), want: "q35"},
	}
	for _, tc := range cases {
		got := machineTypeFor(tc.arch)
		if got != tc.want {
			t.Errorf("machineTypeFor(%q) = %q, want %q", tc.arch, got, tc.want)
		}
	}
}
