// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import "testing"

// TestNeedsCidata_AlwaysOnUnlessDisabled pins the always-on cidata behavior
// change (decision 5): every create attaches a minimal NoCloud seed unless the
// operator explicitly disabled cloud-init. A name-only VM (no user-data, no
// network-config) now gets a seed so the guest hostname follows the VM name.
func TestNeedsCidata_AlwaysOnUnlessDisabled(t *testing.T) {
	tests := []struct {
		name string
		spec CreateSpec
		want bool
	}{
		{name: "empty spec gets seed", spec: CreateSpec{}, want: true},
		{name: "user-data only", spec: CreateSpec{UserData: []byte("#cloud-config\n")}, want: true},
		{name: "network-config only", spec: CreateSpec{NetworkData: []byte("network:\n  version: 2\n")}, want: true},
		{name: "both", spec: CreateSpec{UserData: []byte("x"), NetworkData: []byte("y")}, want: true},
		{name: "explicitly disabled", spec: CreateSpec{CloudInitDisabled: true}, want: false},
		{name: "disabled wins over user-data", spec: CreateSpec{UserData: []byte("x"), CloudInitDisabled: true}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsCidata(tc.spec); got != tc.want {
				t.Errorf("needsCidata(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
