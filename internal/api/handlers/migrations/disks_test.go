// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package migrations

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/internal/agent/cloudinit"
	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/store"
)

func strPtr(s string) *string { return &s }

// TestVMHasCidata mirrors the create-time source of truth
// (resolveCloudInitUserData / resolveCloudInitNetworkConfig in package
// internal/api/handlers/vms): cidata exists iff cloud-init is enabled and at
// least one channel is non-empty.
func TestVMHasCidata(t *testing.T) {
	tests := []struct {
		name string
		vm   store.VM
		want bool
	}{
		{"userdata set, enabled", store.VM{UserData: strPtr("#cloud-config")}, true},
		{"networkconfig set, enabled", store.VM{NetworkConfig: strPtr("version: 2")}, true},
		{"both set, enabled", store.VM{UserData: strPtr("#cloud-config"), NetworkConfig: strPtr("version: 2")}, true},
		{"disabled even with userdata", store.VM{UserData: strPtr("#cloud-config"), CloudInitDisabled: true}, false},
		{"both nil", store.VM{}, false},
		{"userdata pointer to empty", store.VM{UserData: strPtr("")}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := vmHasCidata(tc.vm); got != tc.want {
				t.Errorf("vmHasCidata(%+v) = %v, want %v", tc.vm, got, tc.want)
			}
		})
	}
}

// TestMigrationDisks covers the ordered manifest: boot disk at index 0 always,
// then the fixed-size cidata ISO at index 1 only when the VM has cloud-init.
func TestMigrationDisks(t *testing.T) {
	const bootBytes int64 = 20 << 30

	tests := []struct {
		name       string
		vm         store.VM
		bootFormat string
		want       []agentapi.MigrationDisk
	}{
		{
			name:       "with cloud-init, qcow2 boot",
			vm:         store.VM{UserData: strPtr("#cloud-config")},
			bootFormat: "qcow2",
			want: []agentapi.MigrationDisk{
				{Index: 0, SizeBytes: bootBytes, Format: agentapi.MigrationDiskFormat("qcow2"), ReadOnly: false},
				{Index: 1, SizeBytes: 10485760, Format: agentapi.MigrationDiskFormat("raw"), ReadOnly: true},
			},
		},
		{
			name:       "no cloud-init, qcow2 boot",
			vm:         store.VM{},
			bootFormat: "qcow2",
			want: []agentapi.MigrationDisk{
				{Index: 0, SizeBytes: bootBytes, Format: agentapi.MigrationDiskFormat("qcow2"), ReadOnly: false},
			},
		},
		{
			name:       "no cloud-init, raw boot keeps boot format",
			vm:         store.VM{},
			bootFormat: "raw",
			want: []agentapi.MigrationDisk{
				{Index: 0, SizeBytes: bootBytes, Format: agentapi.MigrationDiskFormat("raw"), ReadOnly: false},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := migrationDisks(tc.vm, bootBytes, tc.bootFormat)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("migrationDisks() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestCidataDefaultDiskSizeNoDrift is the drift guard: the control-plane copy of
// the cidata ISO size MUST equal the agent builder's DefaultDiskSize so a future
// change to the agent's ISO size can never silently desync the manifest.
func TestCidataDefaultDiskSizeNoDrift(t *testing.T) {
	if cloudinitDefaultDiskSize != cloudinit.DefaultDiskSize {
		t.Errorf("cloudinitDefaultDiskSize = %d, want cloudinit.DefaultDiskSize = %d", cloudinitDefaultDiskSize, cloudinit.DefaultDiskSize)
	}
}
