// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import "testing"

func TestNeedsCidata(t *testing.T) {
	tests := []struct {
		name string
		spec CreateSpec
		want bool
	}{
		{name: "empty", spec: CreateSpec{}, want: false},
		{name: "user-data only", spec: CreateSpec{UserData: []byte("#cloud-config\n")}, want: true},
		{name: "network-config only", spec: CreateSpec{NetworkData: []byte("network:\n  version: 2\n")}, want: true},
		{name: "both", spec: CreateSpec{UserData: []byte("x"), NetworkData: []byte("y")}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsCidata(tc.spec); got != tc.want {
				t.Errorf("needsCidata(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
