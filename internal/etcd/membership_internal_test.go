// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"errors"
	"fmt"
	"testing"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
)

func TestIsPeerURLExistErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "peer url exists", err: errors.New("etcdserver: Peer URLs already exists"), want: true},
		{name: "member id exists", err: errors.New("etcdserver: member ID already exist"), want: true},
		{name: "unhealthy cluster", err: errors.New("etcdserver: unhealthy cluster"), want: false},
		// Typed clientv3 sentinels: exercise the errors.Is branch, including when
		// wrapped, so the typed path is covered independent of the message text.
		{name: "typed peer url exists", err: rpctypes.ErrPeerURLExist, want: true},
		{name: "typed member exists wrapped", err: fmt.Errorf("register learner: %w", rpctypes.ErrMemberExist), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPeerURLExistErr(tc.err); got != tc.want {
				t.Errorf("isPeerURLExistErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
