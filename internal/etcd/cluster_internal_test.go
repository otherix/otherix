// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"errors"
	"fmt"
	"testing"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
)

func TestIsTransientReconfigErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "unhealthy cluster", err: errors.New("etcdserver: unhealthy cluster"), want: true},
		{name: "too many learners", err: errors.New("etcdserver: too many learner members in cluster"), want: true},
		{name: "not enough started members", err: errors.New("etcdserver: re-configuration failed due to not enough started members"), want: true},
		{name: "permanent: member not found", err: errors.New("etcdserver: member not found"), want: false},
		{name: "unrelated", err: errors.New("context deadline exceeded"), want: false},
		// Typed clientv3 sentinels: exercise the errors.Is branch, including when
		// wrapped, so the typed path is covered independent of the message text.
		{name: "typed unhealthy", err: rpctypes.ErrUnhealthy, want: true},
		{name: "typed too many learners wrapped", err: fmt.Errorf("register learner: %w", rpctypes.ErrTooManyLearners), want: true},
		{name: "typed not enough started", err: rpctypes.ErrMemberNotEnoughStarted, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientReconfigErr(tc.err); got != tc.want {
				t.Errorf("isTransientReconfigErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
