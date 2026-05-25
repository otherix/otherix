// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package migrations_test

import (
	"context"
	"testing"
	"time"
)

const bytesPerGiB int64 = 1073741824

// poolFixture seeds a node + pool tailored for pool_effective_capacity
// tests. Caller controls `available_bytes` and `reported_at` directly
// (no scan worker involvement); the node row is a minimal placeholder
// — disk subtraction does not key off any node-side column.
type poolFixture struct {
	availableBytes *int64
	reportedAt     *time.Time
}

func seedPoolForEffective(t *testing.T, f poolFixture) (poolID, nodeID string) {
	t.Helper()
	ctx := context.Background()
	suffix := randSlug(t)
	if err := shared.Pool.QueryRow(ctx, `
		insert into nodes
		    (name, architecture, advertised_endpoint, migration_host,
		     migration_port_range_start, migration_port_range_end)
		values ($1, 'amd64', 'https://x', 'x', 49152, 49251)
		returning id`,
		"n-poolff-"+suffix,
	).Scan(&nodeID); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if err := shared.Pool.QueryRow(ctx, `
		insert into storage_pools
		    (node_id, name, type, path, available_bytes, reported_at)
		values ($1, $2, 'local_dir', '/p', $3, $4)
		returning id`,
		nodeID, "pool-eff-"+suffix, f.availableBytes, f.reportedAt,
	).Scan(&poolID); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	return poolID, nodeID
}

// pinDiskArgs is the disk-commitment seed. Caller controls size_gib,
// pool, and optional created_at / deleted_at — every other column gets
// schema defaults. The VM is a throwaway placeholder with unique name;
// device_order increments through pool's slot count so the per-VM
// unique-index does not collide on re-runs.
type pinDiskArgs struct {
	poolID       string
	sizeGiB      int32
	createdAt    *time.Time
	deletedAt    *time.Time
	templateID   *string
	templateName string
}

func pinDisk(t *testing.T, args pinDiskArgs) (diskID, vmID string) {
	t.Helper()
	ctx := context.Background()
	ownerID := seedUser(t)

	// Each disk owns its own VM — keeps the unique
	// `uq_vm_disks_order on vm_disks(vm_id, device_order)` trivially
	// satisfied and lets tests pin multiple disks to the same pool without
	// coordinating device_order.
	if err := shared.Pool.QueryRow(ctx, `
		insert into vms
		    (owner_id, name, architecture, cpu_cores, memory_mib, machine_type)
		values ($1, 'vm-poolff-' || replace(uuid_generate_v7()::text, '-', ''),
		        'amd64', 1, 512, 'q35')
		returning id`,
		ownerID,
	).Scan(&vmID); err != nil {
		t.Fatalf("pinDisk seed vm: %v", err)
	}

	// vm_disks requires a blank-or-template source; pick blank to keep
	// the seed self-contained — no templates table touched.
	if err := shared.Pool.QueryRow(ctx, `
		insert into vm_disks
		    (vm_id, storage_pool_id, device_order, size_gib,
		     source_kind, created_at, deleted_at)
		values ($1, $2, 0, $3, 'blank',
		        coalesce($4, now()), $5)
		returning id`,
		vmID, args.poolID, args.sizeGiB, args.createdAt, args.deletedAt,
	).Scan(&diskID); err != nil {
		t.Fatalf("pinDisk seed disk: %v", err)
	}
	return diskID, vmID
}

// queryPoolEffective reads the view row for poolID. Returns nil-pointer
// when the view emits NULL effective.
func queryPoolEffective(t *testing.T, poolID string) (effective *int64) {
	t.Helper()
	if err := shared.Pool.QueryRow(context.Background(),
		`select available_bytes_effective
		 from pool_effective_capacity where id = $1`,
		poolID,
	).Scan(&effective); err != nil {
		t.Fatalf("query view: %v", err)
	}
	return effective
}

// TestPoolEffective_PendingDiskSubtracted is the core happy path:
// a disk created after the pool's last scan is subtracted from raw
// availability. Mirror of node-iteration's PendingVMSubtracted test.
func TestPoolEffective_PendingDiskSubtracted(t *testing.T) {
	rawAvail := 100 * bytesPerGiB
	poolID, _ := seedPoolForEffective(t, poolFixture{
		availableBytes: i64(rawAvail),
		reportedAt:     ago(1 * time.Hour),
	})
	_, _ = pinDisk(t, pinDiskArgs{
		poolID:  poolID,
		sizeGiB: 10,
		// createdAt defaults to now() → after reported_at → subtracted.
	})

	eff := queryPoolEffective(t, poolID)
	want := rawAvail - 10*bytesPerGiB
	if eff == nil || *eff != want {
		t.Errorf("available_bytes_effective = %v, want *int64(%d)", deref64(eff), want)
	}
}

// TestPoolEffective_PostScanNoDoubleCount confirms the self-correcting
// property: once the agent's scan timestamp advances past the disk's
// created_at, the view stops subtracting (the disk is already
// reflected in available_bytes).
func TestPoolEffective_PostScanNoDoubleCount(t *testing.T) {
	rawAvail := 90 * bytesPerGiB // agent already accounted -10 GiB
	poolID, _ := seedPoolForEffective(t, poolFixture{
		availableBytes: i64(rawAvail),
		reportedAt:     plus(1 * time.Hour), // scan stamp in the future vs disk creation
	})
	_, _ = pinDisk(t, pinDiskArgs{
		poolID:    poolID,
		sizeGiB:   10,
		createdAt: ago(2 * time.Hour), // disk existed before scan
	})

	eff := queryPoolEffective(t, poolID)
	if eff == nil || *eff != rawAvail {
		t.Errorf("available_bytes_effective = %v, want *int64(%d) (no double-count)",
			deref64(eff), rawAvail)
	}
}

// TestPoolEffective_NullReportedAtSubtractsAll covers the bootstrap
// case: pool row exists but has never been scanned (reported_at IS
// NULL). The LATERAL filter takes the `reported_at is null` branch
// and subtracts every committed disk. If available_bytes is also nil
// (real bootstrap state) the effective field stays NULL — that case
// is exercised in NullAvailableBytesPreservesNull below; here we
// pin a stale available_bytes to focus on the timestamp branch.
func TestPoolEffective_NullReportedAtSubtractsAll(t *testing.T) {
	rawAvail := 50 * bytesPerGiB
	poolID, _ := seedPoolForEffective(t, poolFixture{
		availableBytes: i64(rawAvail),
		// reportedAt unset (NULL).
	})
	_, _ = pinDisk(t, pinDiskArgs{poolID: poolID, sizeGiB: 5})

	eff := queryPoolEffective(t, poolID)
	want := rawAvail - 5*bytesPerGiB
	if eff == nil || *eff != want {
		t.Errorf("available_bytes_effective = %v, want *int64(%d)", deref64(eff), want)
	}
}

// TestPoolEffective_FloorAtZero confirms the GREATEST(0, ...) guard:
// pending disks totaling more than raw availability cannot produce
// a negative effective value. Operator sees "0 free" instead.
func TestPoolEffective_FloorAtZero(t *testing.T) {
	poolID, _ := seedPoolForEffective(t, poolFixture{
		availableBytes: i64(5 * bytesPerGiB), // small raw availability
		reportedAt:     ago(1 * time.Hour),
	})
	_, _ = pinDisk(t, pinDiskArgs{poolID: poolID, sizeGiB: 100})

	eff := queryPoolEffective(t, poolID)
	if eff == nil || *eff != 0 {
		t.Errorf("available_bytes_effective = %v, want *int64(0) (floored)", deref64(eff))
	}
}

// TestPoolEffective_MultiplePendingStack verifies the SUM aggregate:
// three pending disks cumulatively reduce effective availability.
func TestPoolEffective_MultiplePendingStack(t *testing.T) {
	rawAvail := 200 * bytesPerGiB
	poolID, _ := seedPoolForEffective(t, poolFixture{
		availableBytes: i64(rawAvail),
		reportedAt:     ago(1 * time.Hour),
	})
	for _, sz := range []int32{10, 20, 30} {
		_, _ = pinDisk(t, pinDiskArgs{poolID: poolID, sizeGiB: sz})
	}

	eff := queryPoolEffective(t, poolID)
	want := rawAvail - (10+20+30)*bytesPerGiB
	if eff == nil || *eff != want {
		t.Errorf("available_bytes_effective = %v, want *int64(%d)", deref64(eff), want)
	}
}

// TestPoolEffective_SoftDeletedDiskNotSubtracted asserts the
// `vd.deleted_at IS NULL` predicate. A soft-deleted disk reflects the
// SoftDeleteVMDisksByVM lifecycle stamp — its space is no longer
// considered committed CP-side.
func TestPoolEffective_SoftDeletedDiskNotSubtracted(t *testing.T) {
	rawAvail := 100 * bytesPerGiB
	poolID, _ := seedPoolForEffective(t, poolFixture{
		availableBytes: i64(rawAvail),
		reportedAt:     ago(1 * time.Hour),
	})
	// Live disk — subtracted.
	_, _ = pinDisk(t, pinDiskArgs{poolID: poolID, sizeGiB: 5})
	// Soft-deleted disk — NOT subtracted.
	_, _ = pinDisk(t, pinDiskArgs{
		poolID:    poolID,
		sizeGiB:   50,
		deletedAt: ago(10 * time.Minute),
	})

	eff := queryPoolEffective(t, poolID)
	want := rawAvail - 5*bytesPerGiB // only the live disk subtracted
	if eff == nil || *eff != want {
		t.Errorf("available_bytes_effective = %v, want *int64(%d) (soft-deleted disk excluded)",
			deref64(eff), want)
	}
}

// TestPoolEffective_DiskInDifferentPoolNotSubtracted asserts per-pool
// scope: a disk pinned to pool A does not appear in pool B's effective.
func TestPoolEffective_DiskInDifferentPoolNotSubtracted(t *testing.T) {
	rawAvail := 100 * bytesPerGiB
	poolA, _ := seedPoolForEffective(t, poolFixture{
		availableBytes: i64(rawAvail),
		reportedAt:     ago(1 * time.Hour),
	})
	poolB, _ := seedPoolForEffective(t, poolFixture{
		availableBytes: i64(rawAvail),
		reportedAt:     ago(1 * time.Hour),
	})
	_, _ = pinDisk(t, pinDiskArgs{poolID: poolA, sizeGiB: 25})

	// Pool A subtracts the disk.
	effA := queryPoolEffective(t, poolA)
	wantA := rawAvail - 25*bytesPerGiB
	if effA == nil || *effA != wantA {
		t.Errorf("poolA effective = %v, want *int64(%d)", deref64(effA), wantA)
	}
	// Pool B unaffected.
	effB := queryPoolEffective(t, poolB)
	if effB == nil || *effB != rawAvail {
		t.Errorf("poolB effective = %v, want *int64(%d) (other pool's disk leaked)",
			deref64(effB), rawAvail)
	}
}

// TestPoolEffective_NullAvailableBytesPreservesNull confirms the
// CASE preserves NULL through to the effective column. Future
// scheduler keys its fallback path off `available_bytes_effective IS
// NULL` (or hasMetrics) — a pool that has never been scanned must
// stay distinguishable from one with zero free bytes.
func TestPoolEffective_NullAvailableBytesPreservesNull(t *testing.T) {
	poolID, _ := seedPoolForEffective(t, poolFixture{
		// Neither availableBytes nor reportedAt set → both NULL.
	})
	_, _ = pinDisk(t, pinDiskArgs{poolID: poolID, sizeGiB: 5})

	eff := queryPoolEffective(t, poolID)
	if eff != nil {
		t.Errorf("available_bytes_effective = %d, want nil (raw NULL → effective NULL)", *eff)
	}
}

// TestPoolEffective_RaceReproduction reproduces the bug Sub-iteration B
// closes: two sequential disk-committing operations within the same
// scan window cumulatively reduce effective availability. Without the
// view, the second placement decision would read a stale
// available_bytes that did not yet reflect the first commit and
// potentially over-allocate.
func TestPoolEffective_RaceReproduction(t *testing.T) {
	rawAvail := 30 * bytesPerGiB
	poolID, _ := seedPoolForEffective(t, poolFixture{
		availableBytes: i64(rawAvail),
		reportedAt:     ago(1 * time.Hour),
	})

	// First create: disk1 = 10 GiB.
	_, _ = pinDisk(t, pinDiskArgs{poolID: poolID, sizeGiB: 10})

	// Immediately re-read view — effective already reflects disk1
	// even though available_bytes has not been re-scanned.
	eff1 := queryPoolEffective(t, poolID)
	want1 := rawAvail - 10*bytesPerGiB
	if eff1 == nil || *eff1 != want1 {
		t.Fatalf("after disk1: effective = %v, want *int64(%d)", deref64(eff1), want1)
	}

	// Second create sees disk1 already accounted for.
	_, _ = pinDisk(t, pinDiskArgs{poolID: poolID, sizeGiB: 15})
	eff2 := queryPoolEffective(t, poolID)
	want2 := rawAvail - (10+15)*bytesPerGiB
	if eff2 == nil || *eff2 != want2 {
		t.Errorf("after disk2: effective = %v, want *int64(%d) (cumulative)",
			deref64(eff2), want2)
	}
}
