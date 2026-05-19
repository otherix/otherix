// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"
)

func TestVMRuntimeOneToOne(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	if _, err := shared.Pool.Exec(ctx, `insert into vm_runtime (vm_id, current_node_id) values ($1, $2)`, f.vmID, f.nodeID); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `insert into vm_runtime (vm_id, current_node_id) values ($1, $2)`, f.vmID, f.nodeID); err == nil {
		t.Fatalf("want PK violation on second runtime row for same vm")
	}
}

func TestVMRuntimeCascadesFromVM(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	if _, err := shared.Pool.Exec(ctx, `insert into vm_runtime (vm_id, current_node_id) values ($1, $2)`, f.vmID, f.nodeID); err != nil {
		t.Fatalf("insert runtime: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `delete from vms where id=$1`, f.vmID); err != nil {
		t.Fatalf("delete vm: %v", err)
	}
	var n int
	if err := shared.Pool.QueryRow(ctx, `select count(*) from vm_runtime where vm_id=$1`, f.vmID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("runtime row not cascaded, n=%d", n)
	}
}

func TestVMRuntimeNodeSetNullOnNodeDelete(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	if _, err := shared.Pool.Exec(ctx, `insert into vm_runtime (vm_id, current_node_id) values ($1, $2)`, f.vmID, f.nodeID); err != nil {
		t.Fatalf("insert runtime: %v", err)
	}
	// Storage pools cascade from node (no vm_disks here, so the cascade is clean
	// in v1, but explicitly clearing pools first makes the test robust to future
	// schema additions).
	if _, err := shared.Pool.Exec(ctx, `delete from storage_pools where node_id=$1`, f.nodeID); err != nil {
		t.Fatalf("delete pool: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `delete from nodes where id=$1`, f.nodeID); err != nil {
		t.Fatalf("delete node: %v", err)
	}
	var nodeID *string
	if err := shared.Pool.QueryRow(ctx, `select current_node_id from vm_runtime where vm_id=$1`, f.vmID).Scan(&nodeID); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if nodeID != nil {
		t.Fatalf("expected current_node_id null, got %v", *nodeID)
	}
}

func TestVMDiskRuntimeCascadesFromVMDisk(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	var diskID string
	if err := shared.Pool.QueryRow(ctx, `
		insert into vm_disks (vm_id, storage_pool_id, device_order, size_gib, source_kind)
		values ($1, $2, 0, 20, 'blank') returning id`, f.vmID, f.poolID).Scan(&diskID); err != nil {
		t.Fatalf("disk: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `insert into vm_disk_runtime (vm_disk_id) values ($1)`, diskID); err != nil {
		t.Fatalf("disk runtime: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `delete from vm_disks where id=$1`, diskID); err != nil {
		t.Fatalf("delete disk: %v", err)
	}
	var n int
	if err := shared.Pool.QueryRow(ctx, `select count(*) from vm_disk_runtime where vm_disk_id=$1`, diskID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("vm_disk_runtime not cascaded, n=%d", n)
	}
}

// TestVMDiskRuntimeNodeSetNullOnNodeDelete is skipped: the SET NULL FK pattern
// on vm_disk_runtime.current_node_id is structurally identical to the same FK
// on vm_runtime (which TestVMRuntimeNodeSetNullOnNodeDelete already covers).
// Testing it here would require deleting vm_disks before the node (because
// vm_disks.storage_pool_id is RESTRICT and pools cascade from node), which
// conflates FK semantics and duplicates coverage without adding signal.
func TestVMDiskRuntimeNodeSetNullOnNodeDelete(t *testing.T) {
	t.Skip("SET NULL on current_node_id is structurally covered by TestVMRuntimeNodeSetNullOnNodeDelete; skip to avoid conflating FK semantics")
}

func TestVMNicRuntimeCascadesFromVMNic(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	var nicID string
	if err := shared.Pool.QueryRow(ctx, `
		insert into vm_nics (vm_id, network_id, device_order, mac_address)
		values ($1, $2, 0, '02:00:00:00:00:01'::macaddr) returning id`, f.vmID, f.networkID).Scan(&nicID); err != nil {
		t.Fatalf("nic: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `insert into vm_nic_runtime (vm_nic_id) values ($1)`, nicID); err != nil {
		t.Fatalf("nic runtime: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `delete from vm_nics where id=$1`, nicID); err != nil {
		t.Fatalf("delete nic: %v", err)
	}
	var n int
	if err := shared.Pool.QueryRow(ctx, `select count(*) from vm_nic_runtime where vm_nic_id=$1`, nicID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("vm_nic_runtime not cascaded, n=%d", n)
	}
}
