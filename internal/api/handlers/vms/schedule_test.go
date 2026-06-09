// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"fmt"
	"testing"

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
