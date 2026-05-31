// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"errors"
	"testing"
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPeerURLExistErr(tc.err); got != tc.want {
				t.Errorf("isPeerURLExistErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
