// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/vm"
)

// fakePoolManager implements PoolManager with optional fault injection.
// AddPool errors fall through via `addError`; RemovePool counts
// invocations via the returned counter.
type fakePoolManager struct {
	mu       sync.Mutex
	pools    map[string]string
	added    []addCall
	removed  []string
	addError error
}

type addCall struct {
	Name string
	Root string
}

func newFakeManager() *fakePoolManager {
	return &fakePoolManager{pools: map[string]string{}}
}

func (f *fakePoolManager) AddPool(name, root string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addError != nil {
		return f.addError
	}
	f.pools[name] = root
	f.added = append(f.added, addCall{Name: name, Root: root})
	return nil
}

func (f *fakePoolManager) RemovePool(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pools, name)
	f.removed = append(f.removed, name)
}

func (f *fakePoolManager) ListPools() []vm.PoolView {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]vm.PoolView, 0, len(f.pools))
	for name, root := range f.pools {
		out = append(out, vm.PoolView{Name: name, Root: root})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (f *fakePoolManager) addedSnapshot() []addCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]addCall, len(f.added))
	copy(out, f.added)
	return out
}

func (f *fakePoolManager) removedSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.removed))
	copy(out, f.removed)
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestNewPools_RejectsNilManager confirms boot-time misconfiguration
// surfaces at construction, not at the first reconcile.
func TestNewPools_RejectsNilManager(t *testing.T) {
	_, err := NewPools(nil, discardLogger(), 0)
	if !errors.Is(err, ErrNilManager) {
		t.Errorf("NewPools(nil, …) error = %v, want ErrNilManager", err)
	}
}

// TestNewPools_DefaultsTick confirms а zero tick falls back to the
// documented default.
func TestNewPools_DefaultsTick(t *testing.T) {
	mgr := newFakeManager()
	rec, err := NewPools(mgr, discardLogger(), 0)
	if err != nil {
		t.Fatalf("NewPools: %v", err)
	}
	if rec.tick != DefaultTickInterval {
		t.Errorf("tick = %v, want %v", rec.tick, DefaultTickInterval)
	}
}

// TestReconcile_AddsNewPool confirms one cycle: declared_pools with а
// pool not in the observed set drives AddPool with the right (name,
// root) и records а `ready` outcome.
func TestReconcile_AddsNewPool(t *testing.T) {
	mgr := newFakeManager()
	rec, _ := NewPools(mgr, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredPools: []heartbeat.DeclaredPool{
			{Name: "alpha", Type: "local_dir", Path: "/opt/otherix/pools/alpha"},
		},
	})
	rec.reconcile(context.Background())

	added := mgr.addedSnapshot()
	want := []addCall{{Name: "alpha", Root: "/opt/otherix/pools/alpha"}}
	if diff := cmp.Diff(want, added); diff != "" {
		t.Errorf("AddPool calls mismatch (-want +got):\n%s", diff)
	}

	reports := rec.PoolReports()
	if len(reports) != 1 || reports[0].Name != "alpha" || reports[0].ReconciliationStatus != "ready" {
		t.Errorf("PoolReports = %+v, want one ready entry", reports)
	}
}

// TestReconcile_RemovesUndeclaredPool confirms that а pool the agent
// observes but the CP no longer declares is unregistered. Filesystem
// state is left intact (assertable only indirectly through ListPools
// dropping the entry).
func TestReconcile_RemovesUndeclaredPool(t *testing.T) {
	mgr := newFakeManager()
	_ = mgr.AddPool("removed", "/opt/otherix/pools/removed")
	rec, _ := NewPools(mgr, discardLogger(), DefaultTickInterval)

	// Empty declared_pools — CP wants this node к hold zero pools.
	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{})
	rec.reconcile(context.Background())

	if got := mgr.removedSnapshot(); len(got) != 1 || got[0] != "removed" {
		t.Errorf("RemovePool calls = %v, want [\"removed\"]", got)
	}
	if reports := rec.PoolReports(); len(reports) != 0 {
		t.Errorf("PoolReports after removal = %+v, want empty", reports)
	}
}

// TestReconcile_FailedAddRecordedAsFailed confirms an AddPool failure
// (e.g. mkdir permission) is captured как а `failed` PoolReport with
// the error message в the report's ReconciliationError field.
func TestReconcile_FailedAddRecordedAsFailed(t *testing.T) {
	mgr := newFakeManager()
	mgr.addError = errors.New("mkdir: permission denied")
	rec, _ := NewPools(mgr, discardLogger(), DefaultTickInterval)

	rec.HandleHeartbeatResponse(context.Background(), &heartbeat.Response{
		DeclaredPools: []heartbeat.DeclaredPool{
			{Name: "broken", Type: "local_dir", Path: "/root/forbidden"},
		},
	})
	rec.reconcile(context.Background())

	reports := rec.PoolReports()
	if len(reports) != 1 {
		t.Fatalf("PoolReports = %+v, want one entry", reports)
	}
	got := reports[0]
	if got.ReconciliationStatus != "failed" {
		t.Errorf("status = %q, want failed", got.ReconciliationStatus)
	}
	if got.ReconciliationError == nil || *got.ReconciliationError != "mkdir: permission denied" {
		t.Errorf("error = %v, want \"mkdir: permission denied\"", got.ReconciliationError)
	}
}

// TestReconcile_RecoversFromFailure confirms а previously-failed pool
// flips к `ready` once the underlying error clears на the next pass.
func TestReconcile_RecoversFromFailure(t *testing.T) {
	mgr := newFakeManager()
	mgr.addError = errors.New("transient")
	rec, _ := NewPools(mgr, discardLogger(), DefaultTickInterval)

	resp := &heartbeat.Response{DeclaredPools: []heartbeat.DeclaredPool{
		{Name: "flaky", Type: "local_dir", Path: "/opt/otherix/pools/flaky"},
	}}
	rec.HandleHeartbeatResponse(context.Background(), resp)
	rec.reconcile(context.Background())

	if got := rec.PoolReports()[0].ReconciliationStatus; got != "failed" {
		t.Fatalf("initial status = %q, want failed", got)
	}

	mgr.mu.Lock()
	mgr.addError = nil
	mgr.mu.Unlock()

	rec.HandleHeartbeatResponse(context.Background(), resp)
	rec.reconcile(context.Background())

	if got := rec.PoolReports()[0].ReconciliationStatus; got != "ready" {
		t.Errorf("post-recovery status = %q, want ready", got)
	}
}

// TestRun_HandlesTriggerAndTick confirms the run loop reconciles на
// trigger AND на ticker, и exits cleanly on ctx cancellation.
func TestRun_HandlesTriggerAndTick(t *testing.T) {
	mgr := newFakeManager()
	rec, _ := NewPools(mgr, discardLogger(), 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	var reconcileCount atomic.Int64

	// Wrap the reconcile method via а tiny ticker observer: each
	// reconcile pass calls AddPool only when desired is non-empty, so
	// counting AddPool calls per declared cycle approximates pass count.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = rec.Run(ctx)
	}()

	// Trigger one reconcile via heartbeat receipt.
	rec.HandleHeartbeatResponse(ctx, &heartbeat.Response{
		DeclaredPools: []heartbeat.DeclaredPool{
			{Name: "a", Type: "local_dir", Path: "/opt/otherix/pools/a"},
		},
	})

	// Wait for at least one AddPool call (trigger-driven).
	deadline := time.After(2 * time.Second)
	for {
		if len(mgr.addedSnapshot()) > 0 {
			reconcileCount.Add(1)
			break
		}
		select {
		case <-deadline:
			t.Fatal("expected trigger-driven AddPool call within 2s")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// Wait for а second AddPool call from the periodic tick (idempotent
	// AddPool keeps getting called every pass).
	deadline = time.After(2 * time.Second)
	for {
		if len(mgr.addedSnapshot()) >= 2 {
			reconcileCount.Add(1)
			break
		}
		select {
		case <-deadline:
			t.Fatal("expected ticker-driven AddPool call within 2s")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	<-done

	if reconcileCount.Load() < 2 {
		t.Errorf("reconcile passes = %d, want ≥ 2 (trigger + ticker)", reconcileCount.Load())
	}
}
