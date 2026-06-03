// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/vm"
)

// fakeVMManager implements VMManager with optional fault injection and
// call recording so the reconciler tests can assert dispatch outcomes
// without touching the production Manager.
type fakeVMManager struct {
	mu       sync.Mutex
	vms      []*vm.VM
	inFlight map[string]struct{}

	starts  []string
	stops   []string
	deletes []string

	startErr error
	stopErr  error
	delErr   error
}

func newFakeVMManager(vms ...*vm.VM) *fakeVMManager {
	return &fakeVMManager{vms: vms, inFlight: map[string]struct{}{}}
}

func (f *fakeVMManager) List() []*vm.VM {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*vm.VM, len(f.vms))
	copy(out, f.vms)
	return out
}

func (f *fakeVMManager) HasInFlight(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.inFlight[name]
	return ok
}

func (f *fakeVMManager) Start(_ context.Context, name string) (*vm.AgentTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, name)
	return nil, f.startErr
}

func (f *fakeVMManager) Stop(_ context.Context, name string) (*vm.AgentTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, name)
	return nil, f.stopErr
}

func (f *fakeVMManager) DeleteByName(_ context.Context, name string) (*vm.AgentTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, name)
	return nil, f.delErr
}

func makeVM(name string, status vm.Status) *vm.VM {
	return &vm.VM{ID: uuid.New(), Name: name, Status: status}
}

func TestNewVMs_RejectsNilManager(t *testing.T) {
	_, err := NewVMs(nil, discardLogger(), 0)
	if !errors.Is(err, ErrNilVMManager) {
		t.Errorf("NewVMs(nil, …) error = %v, want ErrNilVMManager", err)
	}
}

func TestVMs_HandleHeartbeatResponse_TriggersReconcile(t *testing.T) {
	mgr := newFakeVMManager(makeVM("alpha", vm.StatusStopped))
	r, err := NewVMs(mgr, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewVMs: %v", err)
	}
	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredVMs: []heartbeat.DeclaredVM{
			{Name: "alpha", DesiredPhase: "running", Generation: 1},
		},
	})
	r.reconcile(context.Background())

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if got := len(mgr.starts); got != 1 || mgr.starts[0] != "alpha" {
		t.Errorf("starts = %v, want [alpha]", mgr.starts)
	}
}

func TestVMs_Reconcile_TableDriven(t *testing.T) {
	tests := []struct {
		name        string
		observed    []*vm.VM
		declared    []heartbeat.DeclaredVM
		wantStarts  []string
		wantStops   []string
		wantDeletes []string
	}{
		{
			name:       "desired=running observed=stopped → start",
			observed:   []*vm.VM{makeVM("vm-1", vm.StatusStopped)},
			declared:   []heartbeat.DeclaredVM{{Name: "vm-1", DesiredPhase: "running", Generation: 1}},
			wantStarts: []string{"vm-1"},
		},
		{
			name:     "desired=running observed=running → no-op",
			observed: []*vm.VM{makeVM("vm-2", vm.StatusRunning)},
			declared: []heartbeat.DeclaredVM{{Name: "vm-2", DesiredPhase: "running", Generation: 1}},
		},
		{
			name:      "desired=stopped observed=running → stop",
			observed:  []*vm.VM{makeVM("vm-3", vm.StatusRunning)},
			declared:  []heartbeat.DeclaredVM{{Name: "vm-3", DesiredPhase: "stopped", Generation: 1}},
			wantStops: []string{"vm-3"},
		},
		{
			name:     "desired=stopped observed=stopped → no-op",
			observed: []*vm.VM{makeVM("vm-4", vm.StatusStopped)},
			declared: []heartbeat.DeclaredVM{{Name: "vm-4", DesiredPhase: "stopped", Generation: 1}},
		},
		{
			name:        "desired=deleted observed=running → delete",
			observed:    []*vm.VM{makeVM("vm-5", vm.StatusRunning)},
			declared:    []heartbeat.DeclaredVM{{Name: "vm-5", DesiredPhase: "deleted", Generation: 1}},
			wantDeletes: []string{"vm-5"},
		},
		{
			name:     "desired=deleted observed=deleting → no-op",
			observed: []*vm.VM{makeVM("vm-6", vm.StatusDeleting)},
			declared: []heartbeat.DeclaredVM{{Name: "vm-6", DesiredPhase: "deleted", Generation: 1}},
		},
		{
			name:     "observed=failed desired=running → skip (Area 4-IV)",
			observed: []*vm.VM{makeVM("vm-7", vm.StatusFailed)},
			declared: []heartbeat.DeclaredVM{{Name: "vm-7", DesiredPhase: "running", Generation: 1}},
		},
		{
			name:     "observed transitional (stopping) → skip",
			observed: []*vm.VM{makeVM("vm-8", vm.StatusStopping)},
			declared: []heartbeat.DeclaredVM{{Name: "vm-8", DesiredPhase: "stopped", Generation: 1}},
		},
		{
			name:     "observed-but-not-declared (fresh set) → debounce, no prune on first pass",
			observed: []*vm.VM{makeVM("vm-9", vm.StatusRunning)},
			declared: []heartbeat.DeclaredVM{{Name: "other", DesiredPhase: "running", Generation: 1}},
		},
		{
			name:     "declared-but-not-observed → no corrective op",
			observed: nil,
			declared: []heartbeat.DeclaredVM{{Name: "vm-10", DesiredPhase: "running", Generation: 1}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newFakeVMManager(tc.observed...)
			r, err := NewVMs(mgr, discardLogger(), 0)
			if err != nil {
				t.Fatalf("NewVMs: %v", err)
			}
			declared := append([]heartbeat.DeclaredVM(nil), tc.declared...)
			r.desired.Store(&declared)
			r.reconcile(context.Background())

			mgr.mu.Lock()
			defer mgr.mu.Unlock()
			if !sliceEqual(mgr.starts, tc.wantStarts) {
				t.Errorf("starts = %v, want %v", mgr.starts, tc.wantStarts)
			}
			if !sliceEqual(mgr.stops, tc.wantStops) {
				t.Errorf("stops = %v, want %v", mgr.stops, tc.wantStops)
			}
			if !sliceEqual(mgr.deletes, tc.wantDeletes) {
				t.Errorf("deletes = %v, want %v", mgr.deletes, tc.wantDeletes)
			}
		})
	}
}

func TestVMs_Reconcile_SkipsInFlightOps(t *testing.T) {
	v := makeVM("busy", vm.StatusStopped)
	mgr := newFakeVMManager(v)
	mgr.inFlight["busy"] = struct{}{}
	r, err := NewVMs(mgr, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewVMs: %v", err)
	}
	declared := []heartbeat.DeclaredVM{{Name: "busy", DesiredPhase: "running", Generation: 1}}
	r.desired.Store(&declared)
	r.reconcile(context.Background())

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.starts) != 0 {
		t.Errorf("reconciler started a VM with in-flight op: starts=%v", mgr.starts)
	}
}

func TestVMs_VMReports_SortedAndSnapshotted(t *testing.T) {
	v1 := makeVM("z-vm", vm.StatusRunning)
	v2 := makeVM("a-vm", vm.StatusStopped)
	mgr := newFakeVMManager(v1, v2)
	r, err := NewVMs(mgr, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewVMs: %v", err)
	}
	r.reconcile(context.Background())
	reports := r.VMReports()
	if len(reports) != 2 {
		t.Fatalf("got %d reports, want 2", len(reports))
	}
	// Sorted ascending by VMUUID (stable across calls), so just verify
	// every expected VM is present with mapped phase.
	seen := map[uuid.UUID]string{}
	for _, rep := range reports {
		seen[rep.VMUUID] = rep.Phase
	}
	if got := seen[v1.ID]; got != "running" {
		t.Errorf("vm %s phase = %q, want running", v1.Name, got)
	}
	if got := seen[v2.ID]; got != "stopped" {
		t.Errorf("vm %s phase = %q, want stopped", v2.Name, got)
	}
}

// storeDeclared is a test helper that stores a fresh declared set on the
// reconciler, mirroring what HandleHeartbeatResponse does on a successful
// heartbeat.
func storeDeclared(r *VMs, declared ...heartbeat.DeclaredVM) {
	d := append([]heartbeat.DeclaredVM(nil), declared...)
	r.desired.Store(&d)
}

func TestVMs_Prune_TearsDownAfterThreshold(t *testing.T) {
	mgr := newFakeVMManager(makeVM("ghost", vm.StatusRunning))
	r, err := NewVMs(mgr, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewVMs: %v", err)
	}
	// Fresh, non-empty declared set that does NOT include "ghost".
	storeDeclared(r, heartbeat.DeclaredVM{Name: "keep", DesiredPhase: "running", Generation: 1})

	for i := 0; i < pruneThreshold; i++ {
		r.reconcile(context.Background())
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if !sliceEqual(mgr.deletes, []string{"ghost"}) {
		t.Errorf("after %d undeclared passes deletes = %v, want [ghost]", pruneThreshold, mgr.deletes)
	}
}

func TestVMs_Prune_DebouncesBelowThreshold(t *testing.T) {
	mgr := newFakeVMManager(makeVM("ghost", vm.StatusRunning))
	r, err := NewVMs(mgr, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewVMs: %v", err)
	}
	storeDeclared(r, heartbeat.DeclaredVM{Name: "keep", DesiredPhase: "running", Generation: 1})

	for i := 0; i < pruneThreshold-1; i++ {
		r.reconcile(context.Background())
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.deletes) != 0 {
		t.Errorf("pruned before threshold: deletes = %v, want none", mgr.deletes)
	}
}

func TestVMs_Prune_CounterResetsOnReappearance(t *testing.T) {
	mgr := newFakeVMManager(makeVM("ghost", vm.StatusRunning))
	r, err := NewVMs(mgr, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewVMs: %v", err)
	}
	// Absent for K-1 passes.
	storeDeclared(r, heartbeat.DeclaredVM{Name: "keep", DesiredPhase: "running", Generation: 1})
	for i := 0; i < pruneThreshold-1; i++ {
		r.reconcile(context.Background())
	}
	// Reappears: counter must reset.
	storeDeclared(r, heartbeat.DeclaredVM{Name: "ghost", DesiredPhase: "running", Generation: 1})
	r.reconcile(context.Background())
	// Absent again, but only for K-1 fresh passes — must NOT prune yet.
	storeDeclared(r, heartbeat.DeclaredVM{Name: "keep", DesiredPhase: "running", Generation: 1})
	for i := 0; i < pruneThreshold-1; i++ {
		r.reconcile(context.Background())
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.deletes) != 0 {
		t.Errorf("counter did not reset on reappearance: deletes = %v, want none", mgr.deletes)
	}
}

func TestVMs_Prune_NeverOnNilBootDesired(t *testing.T) {
	mgr := newFakeVMManager(makeVM("ghost", vm.StatusRunning))
	r, err := NewVMs(mgr, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewVMs: %v", err)
	}
	// desired stays nil (no heartbeat yet). Many passes must not prune.
	for i := 0; i < pruneThreshold+3; i++ {
		r.reconcile(context.Background())
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.deletes) != 0 {
		t.Errorf("pruned on nil-boot desired: deletes = %v, want none", mgr.deletes)
	}
}

func TestVMs_Prune_NeverWhenInFlight(t *testing.T) {
	mgr := newFakeVMManager(makeVM("ghost", vm.StatusRunning))
	mgr.inFlight["ghost"] = struct{}{}
	r, err := NewVMs(mgr, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewVMs: %v", err)
	}
	storeDeclared(r, heartbeat.DeclaredVM{Name: "keep", DesiredPhase: "running", Generation: 1})
	for i := 0; i < pruneThreshold+3; i++ {
		r.reconcile(context.Background())
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.deletes) != 0 {
		t.Errorf("pruned a mid-create (in-flight) VM: deletes = %v, want none", mgr.deletes)
	}
}

func TestVMs_Prune_NeverOnFailedHeartbeat(t *testing.T) {
	mgr := newFakeVMManager(makeVM("ghost", vm.StatusRunning))
	r, err := NewVMs(mgr, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewVMs: %v", err)
	}
	// A single successful heartbeat declared "ghost"; then heartbeats
	// fail. A failed heartbeat never calls HandleHeartbeatResponse, so
	// desired stays as the last good set that still declares "ghost".
	// Further reconcile passes (ticker-driven) must not prune it, because
	// the stale-but-still-declaring set keeps the absence counter at 0.
	r.HandleHeartbeatResponse(context.Background(),
		&heartbeat.Response{DeclaredVMs: []heartbeat.DeclaredVM{
			{Name: "ghost", DesiredPhase: "running", Generation: 1},
		}})
	for i := 0; i < pruneThreshold+3; i++ {
		r.reconcile(context.Background())
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.deletes) != 0 {
		t.Errorf("pruned on stale-but-declaring desired: deletes = %v, want none", mgr.deletes)
	}
}

func TestVMs_Prune_SkipsAlreadyDeleting(t *testing.T) {
	mgr := newFakeVMManager(makeVM("ghost", vm.StatusDeleting))
	r, err := NewVMs(mgr, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewVMs: %v", err)
	}
	storeDeclared(r, heartbeat.DeclaredVM{Name: "keep", DesiredPhase: "running", Generation: 1})
	for i := 0; i < pruneThreshold+3; i++ {
		r.reconcile(context.Background())
	}

	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.deletes) != 0 {
		t.Errorf("re-issued delete for already-deleting VM: deletes = %v, want none", mgr.deletes)
	}
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
