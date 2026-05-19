// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"strings"
	"testing"
)

type vmFixture struct {
	ownerID, networkID, poolID, nodeID, vmID string
}

// randSlug returns a short hex-derived suffix to make seed names unique
// across tests sharing one container. Uses uuid_generate_v7 for entropy + ms-time.
func randSlug(t *testing.T) string {
	t.Helper()
	var s string
	if err := shared.Pool.QueryRow(context.Background(),
		`select substring(replace(uuid_generate_v7()::text, '-', '') from 1 for 12)`,
	).Scan(&s); err != nil {
		t.Fatalf("randSlug: %v", err)
	}
	return s
}

// seedUser inserts a fresh user with a unique email and returns its id.
// Used by tests that need an owner_id but no full VM fixture.
func seedUser(t *testing.T) string {
	t.Helper()
	suffix := randSlug(t)
	var id string
	if err := shared.Pool.QueryRow(context.Background(),
		`insert into users (email, password_hash, role) values ($1, 'h', 'developer') returning id`,
		"u-"+suffix+"@x.test",
	).Scan(&id); err != nil {
		t.Fatalf("seedUser: %v", err)
	}
	return id
}

func seedVM(t *testing.T) vmFixture {
	t.Helper()
	ctx := context.Background()
	suffix := randSlug(t)
	var f vmFixture
	if err := shared.Pool.QueryRow(ctx, `insert into users (email, password_hash, role) values ($1, 'h', 'developer') returning id`, "u-"+suffix+"@x.test").Scan(&f.ownerID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := shared.Pool.QueryRow(ctx, `insert into nodes (name, architecture, advertised_endpoint, migration_host, migration_port_range_start, migration_port_range_end) values ($1, 'amd64', 'https://x', 'x', 49152, 49251) returning id`, "n-"+suffix).Scan(&f.nodeID); err != nil {
		t.Fatalf("node: %v", err)
	}
	if err := shared.Pool.QueryRow(ctx, `insert into networks (name, type, bridge_name) values ($1, 'bridge', 'br0') returning id`, "net-"+suffix).Scan(&f.networkID); err != nil {
		t.Fatalf("network: %v", err)
	}
	if err := shared.Pool.QueryRow(ctx, `insert into storage_pools (node_id, name, type, path) values ($1, $2, 'local_dir', '/p') returning id`, f.nodeID, "pool-"+suffix).Scan(&f.poolID); err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := shared.Pool.QueryRow(ctx, `
		insert into vms (owner_id, name, architecture, cpu_cores, memory_mib, machine_type)
		values ($1, $2, 'amd64', 2, 2048, 'q35') returning id`,
		f.ownerID, "vm-"+suffix).Scan(&f.vmID); err != nil {
		t.Fatalf("vm: %v", err)
	}
	return f
}

func TestVMNameUnique(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	// Inserting another VM with the same name should fail (globally unique).
	_, err := shared.Pool.Exec(ctx, `
		insert into vms (owner_id, name, architecture, cpu_cores, memory_mib, machine_type)
		select owner_id, name, architecture, cpu_cores, memory_mib, machine_type
		from vms where id=$1`, f.vmID)
	if !isUnique(err) {
		t.Fatalf("want unique violation, got %v", err)
	}
}

func TestVMGenerationBumpsOnCpuChange(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	var g0, g1 int64
	if err := shared.Pool.QueryRow(ctx, `select generation from vms where id=$1`, f.vmID).Scan(&g0); err != nil {
		t.Fatalf("g0: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `update vms set cpu_cores=4 where id=$1`, f.vmID); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := shared.Pool.QueryRow(ctx, `select generation from vms where id=$1`, f.vmID).Scan(&g1); err != nil {
		t.Fatalf("g1: %v", err)
	}
	if g1 != g0+1 {
		t.Fatalf("generation did not bump: g0=%d g1=%d", g0, g1)
	}
}

func TestVMGenerationDoesNotBumpOnDescriptionChange(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	var g0, g1 int64
	if err := shared.Pool.QueryRow(ctx, `select generation from vms where id=$1`, f.vmID).Scan(&g0); err != nil {
		t.Fatalf("g0: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `update vms set description='cosmetic' where id=$1`, f.vmID); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := shared.Pool.QueryRow(ctx, `select generation from vms where id=$1`, f.vmID).Scan(&g1); err != nil {
		t.Fatalf("g1: %v", err)
	}
	if g1 != g0 {
		t.Fatalf("generation should not bump on cosmetic-only change: g0=%d g1=%d", g0, g1)
	}
}

func TestVMDiskBootOrderUnique(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	if _, err := shared.Pool.Exec(ctx, `
		insert into vm_disks (vm_id, storage_pool_id, device_order, size_gib, source_kind)
		values ($1, $2, 0, 20, 'blank')`, f.vmID, f.poolID); err != nil {
		t.Fatalf("d0: %v", err)
	}
	_, err := shared.Pool.Exec(ctx, `
		insert into vm_disks (vm_id, storage_pool_id, device_order, size_gib, source_kind)
		values ($1, $2, 0, 20, 'blank')`, f.vmID, f.poolID)
	if !isUnique(err) {
		t.Fatalf("want unique violation on duplicate device_order, got %v", err)
	}
}

func TestVMDiskSourceCheck(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	// Insert a real template so the FK is satisfied; then try source_kind='blank'
	// with a non-null source_template_id to exercise the CHECK constraint.
	var tplID string
	if err := shared.Pool.QueryRow(ctx, `
		insert into templates (owner_id, name, architecture, os_family, image_url, image_checksum_sha256)
		values ($1, 'tpl-check-'||substr(uuid_generate_v7()::text,1,8), 'amd64', 'linux', 'https://x', '\x00')
		returning id`, f.ownerID).Scan(&tplID); err != nil {
		t.Fatalf("template: %v", err)
	}
	_, err := shared.Pool.Exec(ctx, `
		insert into vm_disks (vm_id, storage_pool_id, device_order, size_gib, source_kind, source_template_id)
		values ($1, $2, 0, 20, 'blank', $3)`, f.vmID, f.poolID, tplID)
	if err == nil {
		t.Fatalf("expected CHECK failure: blank disk cannot have source_template_id")
	}
	// Make sure the error is the CHECK constraint (SQLSTATE 23514), not a FK or other.
	if !strings.Contains(err.Error(), "23514") && !strings.Contains(err.Error(), "chk_vm_disks_template_required") {
		t.Fatalf("expected CHECK 23514 on chk_vm_disks_template_required, got: %v", err)
	}
}

func TestVMDiskGenerationBumpsOnSizeChange(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	var diskID string
	if err := shared.Pool.QueryRow(ctx, `
		insert into vm_disks (vm_id, storage_pool_id, device_order, size_gib, source_kind)
		values ($1, $2, 0, 20, 'blank') returning id`, f.vmID, f.poolID).Scan(&diskID); err != nil {
		t.Fatalf("disk: %v", err)
	}
	var g0, g1 int64
	if err := shared.Pool.QueryRow(ctx, `select generation from vm_disks where id=$1`, diskID).Scan(&g0); err != nil {
		t.Fatalf("g0: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `update vm_disks set size_gib=40 where id=$1`, diskID); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := shared.Pool.QueryRow(ctx, `select generation from vm_disks where id=$1`, diskID).Scan(&g1); err != nil {
		t.Fatalf("g1: %v", err)
	}
	if g1 != g0+1 {
		t.Fatalf("vm_disks generation did not bump on size change: g0=%d g1=%d", g0, g1)
	}
}

func TestVMNicGenerationBumpsOnModelChange(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	var nicID string
	if err := shared.Pool.QueryRow(ctx, `
		insert into vm_nics (vm_id, network_id, device_order, mac_address)
		values ($1, $2, 0, '02:00:00:00:00:02'::macaddr) returning id`, f.vmID, f.networkID).Scan(&nicID); err != nil {
		t.Fatalf("nic: %v", err)
	}
	var g0, g1 int64
	if err := shared.Pool.QueryRow(ctx, `select generation from vm_nics where id=$1`, nicID).Scan(&g0); err != nil {
		t.Fatalf("g0: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `update vm_nics set model='e1000e' where id=$1`, nicID); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := shared.Pool.QueryRow(ctx, `select generation from vm_nics where id=$1`, nicID).Scan(&g1); err != nil {
		t.Fatalf("g1: %v", err)
	}
	if g1 != g0+1 {
		t.Fatalf("vm_nics generation did not bump on model change: g0=%d g1=%d", g0, g1)
	}
}

func TestVMNicMacAddressGloballyUnique(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	mac := "02:00:00:00:00:03"
	if _, err := shared.Pool.Exec(ctx, `
		insert into vm_nics (vm_id, network_id, device_order, mac_address)
		values ($1, $2, 0, $3::macaddr)`, f.vmID, f.networkID, mac); err != nil {
		t.Fatalf("first nic: %v", err)
	}
	// Different VM, same MAC — must violate the global unique constraint.
	g := seedVM(t)
	_, err := shared.Pool.Exec(ctx, `
		insert into vm_nics (vm_id, network_id, device_order, mac_address)
		values ($1, $2, 0, $3::macaddr)`, g.vmID, g.networkID, mac)
	if !isUnique(err) {
		t.Fatalf("want unique violation on duplicate mac_address, got %v", err)
	}
}

func TestSnapshotParentRestrict(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	var rootID string
	if err := shared.Pool.QueryRow(ctx, `insert into snapshots (vm_id, owner_id, name, status, vm_state_at_snapshot) values ($1, $2, 'root', 'ready', 'stopped') returning id`, f.vmID, f.ownerID).Scan(&rootID); err != nil {
		t.Fatalf("root: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `insert into snapshots (vm_id, owner_id, parent_snapshot_id, name, status, vm_state_at_snapshot) values ($1, $2, $3, 'child', 'ready', 'stopped')`, f.vmID, f.ownerID, rootID); err != nil {
		t.Fatalf("child: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `delete from snapshots where id=$1`, rootID); err == nil {
		t.Fatalf("expected RESTRICT on deleting parent snapshot with child")
	}
}

func TestVMDiskBlocksTemplateHardDelete(t *testing.T) {
	ctx := context.Background()
	f := seedVM(t)
	// Create a template and a vm_disk that sources from it.
	var tplID string
	if err := shared.Pool.QueryRow(ctx, `
		insert into templates (owner_id, name, architecture, os_family, image_url, image_checksum_sha256)
		values ($1, 'tpl-'||substr(uuid_generate_v7()::text,1,8), 'amd64', 'linux', 'https://x', '\x00')
		returning id`, f.ownerID).Scan(&tplID); err != nil {
		t.Fatalf("template: %v", err)
	}
	if _, err := shared.Pool.Exec(ctx, `
		insert into vm_disks (vm_id, storage_pool_id, device_order, size_gib, source_kind, source_template_id)
		values ($1, $2, 0, 20, 'template', $3)`, f.vmID, f.poolID, tplID); err != nil {
		t.Fatalf("vm_disk: %v", err)
	}
	// Template hard-delete must be RESTRICTed by the FK from vm_disks.
	_, err := shared.Pool.Exec(ctx, `delete from templates where id=$1`, tplID)
	if err == nil {
		t.Fatalf("expected RESTRICT on template delete while vm_disk references it")
	}
	// Specifically a foreign key violation (23503), not a CHECK violation (23514).
	if !strings.Contains(err.Error(), "23503") && !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("expected FK violation 23503, got: %v", err)
	}
}
