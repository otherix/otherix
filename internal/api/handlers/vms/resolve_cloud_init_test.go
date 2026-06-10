// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"testing"

	"github.com/otherix/otherix/internal/store"
)

// TestResolveCloudInitUserData_ThreeState locks in the VM-only three-state
// semantic (there is no template fallback after the template entity removal):
//
//	cloud_init_disabled=true  → "" (explicit disable wins over vm user_data)
//	vm.user_data set          → override
//	none of the above         → ""
//
// The DB CHECK chk_vms_cloud_init_disabled_no_userdata makes the
// "disabled-AND-userdata" combination unreachable in practice; the
// edge case is exercised here as a defense-in-depth assertion that
// the resolver still chooses disable if both surfaced.
func TestResolveCloudInitUserData_ThreeState(t *testing.T) {
	t.Parallel()

	vmOverride := "#cloud-config\nusers:\n  - name: from-vm\n"

	tests := []struct {
		name       string
		vmUserData *string
		vmDisabled bool
		want       string
	}{
		{
			name: "all-nil → empty",
		},
		{
			name:       "vm override set → vm wins",
			vmUserData: &vmOverride,
			want:       vmOverride,
		},
		{
			name:       "vm empty string → empty treated as absent",
			vmUserData: ptrString(""),
			want:       "",
		},
		{
			name:       "disabled=true + vm override → empty (disable wins, defense-in-depth)",
			vmDisabled: true,
			vmUserData: &vmOverride,
			want:       "",
		},
		{
			name:       "disabled=true alone → empty",
			vmDisabled: true,
			want:       "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vm := store.VM{
				UserData:          tc.vmUserData,
				CloudInitDisabled: tc.vmDisabled,
			}
			got := resolveCloudInitUserData(vm)
			if got != tc.want {
				t.Errorf("resolveCloudInitUserData = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveCloudInitNetworkConfig mirrors the user-data resolver three-state
// semantic for the network-config blob: disabled wins, a set NetworkConfig is
// forwarded verbatim, and a nil column resolves to "".
func TestResolveCloudInitNetworkConfig(t *testing.T) {
	t.Parallel()

	nc := "network:\n  version: 2\n"
	tests := []struct {
		name string
		vm   store.VM
		want string
	}{
		{name: "nil", vm: store.VM{}, want: ""},
		{name: "empty string", vm: store.VM{NetworkConfig: ptrString("")}, want: ""},
		{name: "set", vm: store.VM{NetworkConfig: &nc}, want: nc},
		{name: "disabled wins", vm: store.VM{NetworkConfig: &nc, CloudInitDisabled: true}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveCloudInitNetworkConfig(tc.vm); got != tc.want {
				t.Errorf("resolveCloudInitNetworkConfig(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func ptrString(s string) *string { return &s }
