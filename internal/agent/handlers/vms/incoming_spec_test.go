// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/qemu"
	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/agentapi"
)

func TestIncomingSpecFromRequest(t *testing.T) {
	migID, vmID, nicID := uuid.New(), uuid.New(), uuid.New()
	ab1, ab2 := uuid.New(), uuid.New()
	size := int64(123)
	identity, userData, netCfg := "CN=node-a", "#cloud-config", "version: 2"
	ipv4, mtu := "10.0.0.5", 1450

	base := func() agentapi.MigrationIncomingRequest {
		return agentapi.MigrationIncomingRequest{
			MigrationID: migID,
			Mode:        agentapi.MigrationIncomingRequestModeOffline,
			VMSpec: agentapi.VMSpec{
				VMUUID:       vmID,
				Name:         "demo",
				CPUCores:     2,
				MemoryMib:    1024,
				Architecture: "arm64",
				Disks:        []agentapi.VMSpecDisk{{SizeGib: 10, StoragePoolPath: "/pool"}},
			},
			SourceNodeIdentity: &identity,
		}
	}

	full := base()
	full.ExpectedSizeBytes = &size
	full.UserData = &userData
	full.NetworkConfig = &netCfg
	full.Disks = &[]agentapi.MigrationDisk{
		{Index: 0, SizeBytes: 10 << 30, Format: "qcow2"},
		{Index: 1, SizeBytes: 1 << 20, Format: "raw", ReadOnly: true},
	}
	full.Nics = &[]agentapi.MigrationNic{{
		ID: nicID, BridgeName: "br0", MacAddress: "52:54:00:00:00:01",
		Model: "virtio", DeviceOrder: 1, Mtu: &mtu, Ipv4Address: &ipv4,
	}}
	full.AbandonedMigrationIds = &[]uuid.UUID{ab1, ab2}

	withIDs := base()
	withIDs.AbandonedMigrationIds = &[]uuid.UUID{ab1, ab2}

	tests := []struct {
		name string
		req  agentapi.MigrationIncomingRequest
		want vm.IncomingSpec
	}{
		{
			name: "every field maps",
			req:  full,
			want: vm.IncomingSpec{
				MigrationID:   migID,
				VMUUID:        vmID,
				VMName:        "demo",
				VCPUs:         2,
				MemoryMib:     1024,
				PoolName:      "default",
				Architecture:  qemu.Architecture("arm64"),
				Mode:          "offline",
				ExpectedSize:  123,
				DiskSizeBytes: 10 << 30,
				Disks: []vm.MigrationDisk{
					{Index: 0, SizeBytes: 10 << 30, Format: "qcow2"},
					{Index: 1, SizeBytes: 1 << 20, Format: "raw", ReadOnly: true},
				},
				SourceIdentity: identity,
				BindHost:       "10.1.1.1",
				UserData:       userData,
				NetworkConfig:  netCfg,
				NICs: []netfabric.NIC{{
					ID: nicID, Bridge: "br0", MAC: "52:54:00:00:00:01", Model: "virtio",
					DeviceOrder: 1, MTU: 1450, IPv4: netip.MustParseAddr(ipv4),
				}},
				AbandonedMigrationIDs: []uuid.UUID{ab1, ab2},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := incomingSpecFromRequest(tt.req, "default", "10.1.1.1")
			if diff := cmp.Diff(tt.want, got, cmp.Comparer(func(a, b netip.Addr) bool { return a == b })); diff != "" {
				t.Errorf("incomingSpecFromRequest() mismatch (-want +got):\n%s", diff)
			}
		})
	}

	idTests := []struct {
		name string
		req  agentapi.MigrationIncomingRequest
		want []uuid.UUID
	}{
		{name: "two ids", req: withIDs, want: []uuid.UUID{ab1, ab2}},
		{name: "field absent", req: base(), want: nil},
	}
	for _, tt := range idTests {
		t.Run(tt.name, func(t *testing.T) {
			got := incomingSpecFromRequest(tt.req, "default", "10.1.1.1").AbandonedMigrationIDs
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("incomingSpecFromRequest().AbandonedMigrationIDs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
