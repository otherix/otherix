// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// TestSchedulingReasonFor locks the bind-error -> pending-reason mapping so a
// new scheduler sentinel cannot silently collapse into the pool_not_ready
// default. Each row wraps the sentinel with %w (as SchedulePlacement does) to
// prove the mapping routes on errors.Is/As, not on identity.
func TestSchedulingReasonFor(t *testing.T) {
	t.Parallel()

	netName := "net0"
	spec := store.SchedulingSpec{PoolName: "default", NetworkName: &netName}

	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "network not found", err: fmt.Errorf("bind: %w", errNetworkNotFound), want: store.SchedReasonNetworkNotFound},
		{name: "pool not found", err: fmt.Errorf("bind: %w", scheduler.ErrPoolNotFound), want: store.SchedReasonPoolNotFound},
		{name: "pool not on node", err: fmt.Errorf("bind: %w", scheduler.ErrPoolNotOnNode), want: store.SchedReasonPoolNotOnNode},
		{name: "no eligible nodes", err: fmt.Errorf("bind: %w", scheduler.ErrNoEligibleNodes), want: store.SchedReasonNoEligibleNodes},
		{name: "pool not writable", err: &poolNotWritableError{poolType: "ceph_rbd"}, want: store.SchedReasonPoolNotWritable},
		{name: "subnet exhausted", err: fmt.Errorf("bind: %w", store.ErrSubnetExhausted), want: store.SchedReasonSubnetExhausted},
		{name: "unmapped infra error falls to pool_not_ready", err: fmt.Errorf("etcd timeout"), want: store.SchedReasonPoolNotReady},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _, _ := schedulingReasonFor(tc.err, spec)
			if got != tc.want {
				t.Errorf("schedulingReasonFor(%s) reason = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

type scheduleLockSpy struct {
	mu     sync.Mutex
	events []string
	keys   []int64
}

func (sp *scheduleLockSpy) AcquirePlacementLock(ctx context.Context, lockKey int64) (func(), error) {
	sp.mu.Lock()
	sp.events = append(sp.events, "lock")
	sp.keys = append(sp.keys, lockKey)
	sp.mu.Unlock()
	return func() {
		sp.mu.Lock()
		sp.events = append(sp.events, "unlock")
		sp.mu.Unlock()
	}, nil
}

// BindScheduledVM records the commit point. It deliberately does NOT run plan:
// this test asserts only the lock-vs-bind ordering, so a real placement read is
// unnecessary (and would need a full store).
func (sp *scheduleLockSpy) BindScheduledVM(ctx context.Context, vmID uuid.UUID, plan func(store.PlacementReader) (store.VMBindWrites, error)) error {
	sp.mu.Lock()
	sp.events = append(sp.events, "bind")
	sp.mu.Unlock()
	return nil
}

func (sp *scheduleLockSpy) ListUnscheduledVMs(ctx context.Context, limit int) ([]store.VM, error) {
	return nil, nil
}

func (sp *scheduleLockSpy) UpdateVMSchedulingReason(ctx context.Context, vmID uuid.UUID, reason, message string, details []byte) error {
	return nil
}

// TestBindWithMACRetry_HoldsPlacementLockAcrossBind pins the structural fix: the
// placement lock wraps the whole BindScheduledVM call (which commits the pin
// AFTER the plan callback returns), not just the plan callback. Order must be
// [lock, bind, unlock].
func TestBindWithMACRetry_HoldsPlacementLockAcrossBind(t *testing.T) {
	spy := &scheduleLockSpy{}
	vm := store.VM{ID: uuid.New(), CpuCores: 1, MemoryMib: 512}
	spec := store.SchedulingSpec{PoolName: "default"}
	if err := bindWithMACRetry(context.Background(), spy, vm, spec, ScheduleConfig{}, scheduler.ResourcesConfig{}); err != nil {
		t.Fatalf("bindWithMACRetry = %v, want nil", err)
	}
	want := []string{"lock", "bind", "unlock"}
	if !slices.Equal(spy.events, want) {
		t.Errorf("call order = %v, want %v (lock wraps BindScheduledVM, released after commit)", spy.events, want)
	}
	if len(spy.keys) != 1 || spy.keys[0] != store.LockKeyPlacement {
		t.Errorf("lock keys = %v, want [%d]", spy.keys, store.LockKeyPlacement)
	}
}
