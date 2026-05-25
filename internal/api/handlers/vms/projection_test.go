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
		name    string
		vm      store.VM
		runtime *store.VMRuntime
		want    string
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
			got := projectStatus(tc.vm, tc.runtime)
			if got != tc.want {
				t.Errorf("projectStatus(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

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
