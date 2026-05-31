// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"testing"

	"github.com/otherix/otherix/internal/store"
)

func TestLifecycleKinds(t *testing.T) {
	want := []LifecycleKind{
		{Kind: "vm.start", Op: "start", DesiredPhase: store.VmDesiredPhaseRunning, RuntimePhase: store.VmPhaseRunning, FailureCode: errCodeVMStartFailed},
		{Kind: "vm.stop", Op: "stop", DesiredPhase: store.VmDesiredPhaseStopped, RuntimePhase: store.VmPhaseStopped, FailureCode: errCodeVMStopFailed},
		{Kind: "vm.poweroff", Op: "poweroff", DesiredPhase: store.VmDesiredPhaseStopped, RuntimePhase: store.VmPhaseStopped, FailureCode: errCodeVMPoweroffFailed},
		{Kind: "vm.reboot", Op: "reboot", DesiredPhase: store.VMDesiredPhase(""), RuntimePhase: store.VmPhaseRunning, FailureCode: errCodeVMRebootFailed},
	}
	got := LifecycleKinds()
	if len(got) != len(want) {
		t.Fatalf("LifecycleKinds() len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("LifecycleKinds()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
