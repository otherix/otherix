// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"testing"

	"github.com/otherix/otherix/internal/store"
)

// TestResolveCloudInitUserData_ThreeState locks in the three-state
// semantic the operator UX iteration introduced:
//
//	cloud_init_disabled=true     → "" (explicit disable wins over both
//	                                    VM user_data и template baked)
//	vm.user_data set             → override
//	template.cloud_init_user_data → fallback
//	none of the above            → ""
//
// The DB CHECK chk_vms_cloud_init_disabled_no_userdata makes the
// "disabled-AND-userdata" combination unreachable in practice; the
// edge case is exercised here as а defense-in-depth assertion that
// the resolver still chooses disable если both surfaced (e.g. via а
// schema drift bug or future migration mishap).
func TestResolveCloudInitUserData_ThreeState(t *testing.T) {
	t.Parallel()

	templateBaked := "#cloud-config\npackages:\n  - htop\n"
	vmOverride := "#cloud-config\nusers:\n  - name: from-vm\n"

	tests := []struct {
		name             string
		vmUserData       *string
		vmDisabled       bool
		templateUserData *string
		want             string
	}{
		{
			name: "all-nil → empty",
		},
		{
			name:             "template-only baked → template fallback",
			templateUserData: &templateBaked,
			want:             templateBaked,
		},
		{
			name:             "vm override + template baked → vm wins",
			vmUserData:       &vmOverride,
			templateUserData: &templateBaked,
			want:             vmOverride,
		},
		{
			name:       "vm override + template absent → vm wins",
			vmUserData: &vmOverride,
			want:       vmOverride,
		},
		{
			name:             "vm empty string + template baked → empty treated as absent, template fallback",
			vmUserData:       ptrString(""),
			templateUserData: &templateBaked,
			want:             templateBaked,
		},
		{
			name:             "disabled=true + template baked → empty (disable wins)",
			vmDisabled:       true,
			templateUserData: &templateBaked,
			want:             "",
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
			vm := store.Vm{
				UserData:          tc.vmUserData,
				CloudInitDisabled: tc.vmDisabled,
			}
			tpl := store.Template{
				CloudInitUserData: tc.templateUserData,
			}
			got := resolveCloudInitUserData(vm, tpl)
			if got != tc.want {
				t.Errorf("resolveCloudInitUserData = %q, want %q", got, tc.want)
			}
		})
	}
}

func ptrString(s string) *string { return &s }
