// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/vm"
)

// fakeVMManager implements VMManager with optional fault injection and
// call recording so the reconciler tests can assert dispatch outcomes
// without touching the production Manager.
type fakeVMManager struct {
	mu        sync.Mutex
	vms       []*vm.VM
	inFlight  map[string]struct{}
	migrating map[uuid.UUID]struct{}

	starts      []string
	stops       []string
	deletes     []string
	deletesByID []uuid.UUID

	// calls records method names in invocation order, so a test can assert the
	// relative order of two manager calls within one reconcile pass.
	calls []string

	startErr      error
	stopErr       error
	delErr        error
	deleteByIDErr error

	memUsedMiB *int64
}

func newFakeVMManager(vms ...*vm.VM) *fakeVMManager {
	return &fakeVMManager{
		vms:       vms,
		inFlight:  map[string]struct{}{},
		migrating: map[uuid.UUID]struct{}{},
	}
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

// Delete records the UUID it was asked to tear down. On success it drops the
// VM from the fake's list, the way a settled teardown drops it from the real
// manager, so the terminate round trip is observable.
func (f *fakeVMManager) Delete(_ context.Context, vmID uuid.UUID) (*vm.AgentTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletesByID = append(f.deletesByID, vmID)
	f.calls = append(f.calls, "Delete")
	if f.deleteByIDErr != nil {
		return nil, f.deleteByIDErr
	}
	for i, v := range f.vms {
		if v.ID == vmID {
			f.vms = append(f.vms[:i:i], f.vms[i+1:]...)
			break
		}
	}
	return nil, nil
}

func (f *fakeVMManager) HasActiveMigration(vmID uuid.UUID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.migrating[vmID]
	return ok
}

func (f *fakeVMManager) GuestMemUsedMiB(_ string) *int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "GuestMemUsedMiB")
	return f.memUsedMiB
}

func i64ptr(v int64) *int64 { return &v }

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
			// Live-migration target post-cutover: the adopted VM is
			// StatusMigratingIncoming, not StatusStopped, so the reconciler
			// must NOT cold-start it (the resume driver flips it to running).
			name:     "observed migrating_incoming desired=running → skip (no cold start)",
			observed: []*vm.VM{makeVM("vm-11", vm.StatusMigratingIncoming)},
			declared: []heartbeat.DeclaredVM{{Name: "vm-11", DesiredPhase: "running", Generation: 1}},
		},
		{
			name:     "observed-but-not-declared → no corrective op",
			observed: []*vm.VM{makeVM("vm-9", vm.StatusRunning)},
			declared: nil,
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

func TestReconcile_PopulatesMemoryUsedForRunningVMs(t *testing.T) {
	running := makeVM("run-vm", vm.StatusRunning)
	stopped := makeVM("stop-vm", vm.StatusStopped)
	mgr := newFakeVMManager(running, stopped)
	mgr.memUsedMiB = i64ptr(700)
	r, err := NewVMs(mgr, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewVMs: %v", err)
	}
	r.reconcile(context.Background())

	byUUID := map[uuid.UUID]heartbeat.VMReport{}
	for _, rep := range r.VMReports() {
		byUUID[rep.VMUUID] = rep
	}
	if got := byUUID[running.ID].MemoryUsedMib; got == nil || *got != 700 {
		t.Errorf("running VM MemoryUsedMib = %v, want 700", got)
	}
	if got := byUUID[stopped.ID].MemoryUsedMib; got != nil {
		t.Errorf("stopped VM MemoryUsedMib = %v, want nil", got)
	}
}

// TestMapPhase locks the agent-side vm.Status -> wire heartbeat phase
// mapping. The migrating_incoming case in particular reports the new
// migrating wire phase so the CP projection shows migrating (not
// creating) during the post-cutover tail while the target still holds
// the incoming VM.
func TestMapPhase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   vm.Status
		want string
	}{
		{vm.StatusPending, "pending"},
		{vm.StatusCreating, "pending"},
		{vm.StatusRunning, "running"},
		{vm.StatusPaused, "paused"},
		{vm.StatusStopping, "stopped"},
		{vm.StatusStopped, "stopped"},
		{vm.StatusDeleting, "stopped"},
		{vm.StatusMigratingIncoming, "migrating"},
		{vm.StatusFailed, "error"},
	}
	for _, tc := range cases {
		if got := mapPhase(tc.in); got != tc.want {
			t.Errorf("mapPhase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// handleAndReconcile drives the real entry sequence: the heartbeat response
// handler (which copies the tombstone slice and nudges the trigger) followed
// by one reconcile pass. Tests never write the tombstone cache directly - the
// handler is the seam under test.
func handleAndReconcile(t *testing.T, r *VMs, resp *heartbeat.Response) {
	t.Helper()
	ctx := context.Background()
	r.HandleHeartbeatResponse(ctx, resp)
	r.reconcile(ctx)
}

func newVMsForTest(t *testing.T, mgr VMManager) *VMs {
	t.Helper()
	r, err := NewVMs(mgr, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewVMs: %v", err)
	}
	return r
}

func deletedIDs(f *fakeVMManager) []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uuid.UUID(nil), f.deletesByID...)
}

// TestReconcile_TombstonedVMKnownToManagerIsDeleted asserts a tombstone naming
// a VM the manager holds dispatches Delete by UUID.
func TestReconcile_TombstonedVMKnownToManagerIsDeleted(t *testing.T) {
	v := makeVM("doomed", vm.StatusRunning)
	mgr := newFakeVMManager(v)
	r := newVMsForTest(t, mgr)

	r.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		VMTombstones: []heartbeat.VMTombstone{{VMID: v.ID, VMName: "doomed"}},
	})
	if len(r.trigger) != 1 {
		t.Errorf("HandleHeartbeatResponse did not nudge the trigger: len = %d, want 1", len(r.trigger))
	}
	r.reconcile(context.Background())

	got := deletedIDs(mgr)
	if len(got) != 1 || got[0] != v.ID {
		t.Errorf("Delete calls = %v, want [%v]", got, v.ID)
	}
}

// TestReconcile_TombstonedVMAbsentFromManagerIsANoOp asserts a tombstone for a
// VM the manager does not know dispatches nothing - there is nothing to tear
// down, and the control plane simply stops sending it once the node stops
// reporting the VM.
func TestReconcile_TombstonedVMAbsentFromManagerIsANoOp(t *testing.T) {
	mgr := newFakeVMManager(makeVM("bystander", vm.StatusRunning))
	r := newVMsForTest(t, mgr)

	handleAndReconcile(t, r, &heartbeat.Response{
		VMTombstones: []heartbeat.VMTombstone{{VMID: uuid.New(), VMName: "ghost"}},
	})

	if got := deletedIDs(mgr); len(got) != 0 {
		t.Errorf("Delete calls = %v, want none", got)
	}
}

// TestReconcile_TombstonedVMInFlightIsSkipped asserts a tombstone for a VM with
// a lifecycle op in flight is skipped this pass rather than racing it.
func TestReconcile_TombstonedVMInFlightIsSkipped(t *testing.T) {
	v := makeVM("busy", vm.StatusRunning)
	mgr := newFakeVMManager(v)
	mgr.inFlight["busy"] = struct{}{}
	r := newVMsForTest(t, mgr)

	handleAndReconcile(t, r, &heartbeat.Response{
		VMTombstones: []heartbeat.VMTombstone{{VMID: v.ID, VMName: "busy"}},
	})

	if got := deletedIDs(mgr); len(got) != 0 {
		t.Errorf("Delete calls = %v, want none (op in flight)", got)
	}
}

// TestReconcile_TombstonedVMWithActiveMigrationIsSkipped asserts a tombstone for
// a VM with a non-terminal migration record dispatches nothing, for EITHER
// role. Manager.Delete is not serialised against the migration state machine,
// so tearing down here would SIGKILL a source mid-RAM-stream and remove its
// disk while the block mirror still exports it, or kill an incoming guest whose
// dest disk may be the only copy. The source case needs its own assertion:
// there is no migrating_outgoing status, so a source stays StatusRunning and a
// status check alone would not catch it.
func TestReconcile_TombstonedVMWithActiveMigrationIsSkipped(t *testing.T) {
	tests := []struct {
		name            string
		status          vm.Status
		activeMigration bool
	}{
		{
			name:            "source stays running, only the migration record reveals it",
			status:          vm.StatusRunning,
			activeMigration: true,
		},
		{
			name:            "target holds the incoming guest",
			status:          vm.StatusMigratingIncoming,
			activeMigration: true,
		},
		{
			name:            "incoming status with no record yet",
			status:          vm.StatusMigratingIncoming,
			activeMigration: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := makeVM("mig", tc.status)
			mgr := newFakeVMManager(v)
			if tc.activeMigration {
				mgr.migrating[v.ID] = struct{}{}
			}
			r := newVMsForTest(t, mgr)

			handleAndReconcile(t, r, &heartbeat.Response{
				VMTombstones: []heartbeat.VMTombstone{{VMID: v.ID, VMName: "mig"}},
			})

			if got := deletedIDs(mgr); len(got) != 0 {
				t.Errorf("Delete calls = %v, want none (migration in progress)", got)
			}
		})
	}
}

// TestReconcile_TombstonedDeletingVMIsRedriven asserts a VM replayed at
// StatusDeleting - the crash-mid-teardown wedge - IS torn down rather than
// skipped as transitional. A blanket transitional skip would reintroduce
// exactly the hole this feature closes.
func TestReconcile_TombstonedDeletingVMIsRedriven(t *testing.T) {
	v := makeVM("wedged", vm.StatusDeleting)
	mgr := newFakeVMManager(v)
	r := newVMsForTest(t, mgr)

	handleAndReconcile(t, r, &heartbeat.Response{
		VMTombstones: []heartbeat.VMTombstone{{VMID: v.ID, VMName: "wedged"}},
	})

	got := deletedIDs(mgr)
	if len(got) != 1 || got[0] != v.ID {
		t.Errorf("Delete calls = %v, want [%v] (deleting must be re-driven)", got, v.ID)
	}
}

// TestReconcile_FailedTeardownIsNotRedispatchedInsideTheRetryInterval asserts
// the pass does not re-enter Manager.Delete every tick after a failure, which
// would be a permanent SIGKILL loop on a stuck pid, and that it DOES
// re-dispatch once the interval elapses.
func TestReconcile_FailedTeardownIsNotRedispatchedInsideTheRetryInterval(t *testing.T) {
	v := makeVM("stuck", vm.StatusRunning)
	mgr := newFakeVMManager(v)
	mgr.deleteByIDErr = errors.New("qemu pid will not die")
	r := newVMsForTest(t, mgr)

	resp := &heartbeat.Response{
		VMTombstones: []heartbeat.VMTombstone{{VMID: v.ID, VMName: "stuck"}},
	}
	handleAndReconcile(t, r, resp)
	if got := len(deletedIDs(mgr)); got != 1 {
		t.Fatalf("Delete calls after first pass = %d, want 1", got)
	}

	// A second heartbeat re-sends the same tombstone: still inside the
	// interval, so no second attempt.
	handleAndReconcile(t, r, resp)
	if got := len(deletedIDs(mgr)); got != 1 {
		t.Errorf("Delete calls inside the retry interval = %d, want 1", got)
	}

	// Once the interval has elapsed the transient failure gets another try -
	// the brake must never become "never retry".
	r.teardownMu.Lock()
	r.lastTeardownTry[v.ID] = time.Now().Add(-2 * teardownRetryInterval)
	r.teardownMu.Unlock()
	handleAndReconcile(t, r, resp)
	if got := len(deletedIDs(mgr)); got != 2 {
		t.Errorf("Delete calls after the retry interval elapsed = %d, want 2", got)
	}

	// A response that no longer names the VM prunes its attempt record, so
	// the map cannot grow without bound.
	handleAndReconcile(t, r, &heartbeat.Response{})
	r.teardownMu.Lock()
	defer r.teardownMu.Unlock()
	if got := len(r.lastTeardownTry); got != 0 {
		t.Errorf("lastTeardownTry len after the tombstone cleared = %d, want 0", got)
	}
}

// TestReconcile_TombstoneTerminates is the round trip the whole design rests
// on: a tombstoned VM is torn down, leaves the manager's list, and is therefore
// absent from the next report - so the control plane stops sending the
// tombstone and the loop ends on its own.
func TestReconcile_TombstoneTerminates(t *testing.T) {
	v := makeVM("last-call", vm.StatusRunning)
	mgr := newFakeVMManager(v)
	r := newVMsForTest(t, mgr)

	handleAndReconcile(t, r, &heartbeat.Response{
		VMTombstones: []heartbeat.VMTombstone{{VMID: v.ID, VMName: "last-call"}},
	})
	if got := len(deletedIDs(mgr)); got != 1 {
		t.Fatalf("Delete calls = %d, want 1", got)
	}

	// The next pass reports nothing, which is what stops the CP re-sending.
	r.reconcile(context.Background())
	if reports := r.VMReports(); len(reports) != 0 {
		t.Errorf("VMReports after teardown = %v, want empty", reports)
	}
	if got := len(deletedIDs(mgr)); got != 1 {
		t.Errorf("Delete calls after teardown = %d, want 1 (no re-dispatch)", got)
	}
}

// TestReconcile_UndeclaredVMWithoutTombstoneIsNeverTornDown is the guard for
// the design's central safety property: a VM the manager holds that is absent
// from declared_vms, with no tombstone naming it, is reported and left running.
// declared_vms is a fail-open producer - the agent receives a nil declaration
// on every boot before the first response lands - so absence must never be
// destructive.
func TestReconcile_UndeclaredVMWithoutTombstoneIsNeverTornDown(t *testing.T) {
	v := makeVM("survivor", vm.StatusRunning)
	mgr := newFakeVMManager(v)
	r := newVMsForTest(t, mgr)

	// An empty response: no declaration, no tombstone - exactly the shape the
	// agent sees on boot before the CP has said anything about this node.
	handleAndReconcile(t, r, &heartbeat.Response{})

	if got := deletedIDs(mgr); len(got) != 0 {
		t.Errorf("Delete calls = %v, want none", got)
	}
	mgr.mu.Lock()
	byName := append([]string(nil), mgr.deletes...)
	mgr.mu.Unlock()
	if len(byName) != 0 {
		t.Errorf("DeleteByName calls = %v, want none", byName)
	}
	reports := r.VMReports()
	if len(reports) != 1 || reports[0].VMUUID != v.ID {
		t.Errorf("VMReports = %v, want the surviving VM %v reported", reports, v.ID)
	}
}

// TestReconcile_TeardownRunsBeforeTheObservedStateSweep pins the ordering inside
// one reconcile pass: teardown of a tombstoned VM is dispatched BEFORE the loop
// that reads guest memory stats. That loop does a QMP round trip per running VM,
// so on a hung socket it can stall the whole pass; a destructive correction the
// control plane has already committed must not queue behind an unrelated stats
// read. Moving the teardown call below the reports loop would leave every other
// assertion in this file green, so the order needs its own guard.
func TestReconcile_TeardownRunsBeforeTheObservedStateSweep(t *testing.T) {
	v := makeVM("doomed", vm.StatusRunning)
	mgr := newFakeVMManager(v)
	r := newVMsForTest(t, mgr)

	handleAndReconcile(t, r, &heartbeat.Response{
		VMTombstones: []heartbeat.VMTombstone{{VMID: v.ID, VMName: "doomed"}},
	})

	mgr.mu.Lock()
	got := append([]string(nil), mgr.calls...)
	mgr.mu.Unlock()
	want := []string{"Delete", "GuestMemUsedMiB"}
	if !sliceEqual(got, want) {
		t.Errorf("manager call order = %v, want %v (teardown must not queue behind the stats read)", got, want)
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
