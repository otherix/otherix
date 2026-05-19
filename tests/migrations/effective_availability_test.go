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

// effectiveFixture seeds а user + node tailored для node_effective_
// availability tests: caller controls cpu_cores_{total,available},
// memory_{total,available}_mib, и last_heartbeat_at. Returns
// (ownerID, nodeID).
type effectiveFixture struct {
	cpuTotal     *int32
	cpuAvailable *int32
	memTotalMib  *int64
	memAvailMib  *int64
	lastHB       *time.Time
}

func seedNodeForEffective(t *testing.T, f effectiveFixture) (ownerID, nodeID string) {
	t.Helper()
	ctx := context.Background()
	ownerID = seedUser(t)
	suffix := randSlug(t)
	if err := shared.Pool.QueryRow(ctx, `
		insert into nodes
		    (name, architecture, advertised_endpoint, migration_host,
		     migration_port_range_start, migration_port_range_end,
		     cpu_cores_total, cpu_cores_available,
		     memory_total_mib, memory_available_mib,
		     last_heartbeat_at)
		values ($1, 'amd64', 'https://x', 'x', 49152, 49251,
		        $2, $3, $4, $5, $6)
		returning id`,
		"n-eff-"+suffix,
		f.cpuTotal, f.cpuAvailable, f.memTotalMib, f.memAvailMib, f.lastHB,
	).Scan(&nodeID); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return ownerID, nodeID
}

// pinVM inserts а VM pinned к the supplied node, с the supplied
// resource shape и optional createdAt / desiredPhase / deletedAt
// overrides. defaults: desired_phase='running', created_at=now(),
// deleted_at=NULL.
type pinVMArgs struct {
	ownerID      string
	nodeID       string
	cpuCores     int32
	memoryMib    int32
	desiredPhase string     // "" → 'running'
	createdAt    *time.Time // nil → now()
	deletedAt    *time.Time // nil → NULL
}

func pinVM(t *testing.T, args pinVMArgs) string {
	t.Helper()
	desired := args.desiredPhase
	if desired == "" {
		desired = "running"
	}
	// Construct а fully random name via uuid_generate_v7 — randSlug
	// alone returns only the v7 timestamp prefix, which collides for
	// pins issued in the same millisecond (multi-VM tests).
	var id string
	if err := shared.Pool.QueryRow(context.Background(), `
		insert into vms
		    (owner_id, name, architecture, cpu_cores, memory_mib,
		     machine_type, pinned_node_id, desired_phase,
		     created_at, deleted_at)
		values ($1, 'vm-eff-' || replace(uuid_generate_v7()::text, '-', ''),
		        'amd64', $2, $3, 'q35', $4, $5,
		        coalesce($6, now()), $7)
		returning id`,
		args.ownerID, args.cpuCores, args.memoryMib,
		args.nodeID, desired, args.createdAt, args.deletedAt,
	).Scan(&id); err != nil {
		t.Fatalf("pin vm: %v", err)
	}
	return id
}

// queryEffective reads the view row for nodeID и returns the effective
// columns. Nil pointers when the view emits NULL.
func queryEffective(t *testing.T, nodeID string) (cpuEff *int32, memEff *int64) {
	t.Helper()
	if err := shared.Pool.QueryRow(context.Background(),
		`select cpu_cores_effective, memory_effective_mib
		 from node_effective_availability where id = $1`,
		nodeID,
	).Scan(&cpuEff, &memEff); err != nil {
		t.Fatalf("query view: %v", err)
	}
	return cpuEff, memEff
}

func i32(v int32) *int32              { return &v }
func i64(v int64) *int64              { return &v }
func ts(t time.Time) *time.Time       { return &t }
func plus(d time.Duration) *time.Time { v := time.Now().Add(d); return &v }
func ago(d time.Duration) *time.Time  { v := time.Now().Add(-d); return &v }

// TestEffectiveAvailability_PendingVMSubtracted is the core happy path:
// а VM created after the node's last heartbeat is subtracted from raw
// availability. This is the race window the view closes.
func TestEffectiveAvailability_PendingVMSubtracted(t *testing.T) {
	_, nodeID := seedNodeForEffective(t, effectiveFixture{
		cpuTotal: i32(8), cpuAvailable: i32(8),
		memTotalMib: i64(16384), memAvailMib: i64(16384),
		lastHB: ago(1 * time.Hour), // heartbeat older than any VM we pin
	})
	ownerID := seedUser(t)
	_ = pinVM(t, pinVMArgs{
		ownerID: ownerID, nodeID: nodeID,
		cpuCores: 4, memoryMib: 8192,
		// createdAt defaults к now() → after last heartbeat → subtracted.
	})

	cpuEff, memEff := queryEffective(t, nodeID)
	if cpuEff == nil || *cpuEff != 4 {
		t.Errorf("cpu_cores_effective = %v, want *int32(4)", deref32(cpuEff))
	}
	if memEff == nil || *memEff != 8192 {
		t.Errorf("memory_effective_mib = %v, want *int64(8192)", deref64(memEff))
	}
}

// TestEffectiveAvailability_PostHeartbeatNoDoubleCount confirms the
// self-correcting property: once а heartbeat arrives newer than the
// VM's created_at, the view stops subtracting (agent's report already
// accounts для it).
func TestEffectiveAvailability_PostHeartbeatNoDoubleCount(t *testing.T) {
	ownerID, nodeID := seedNodeForEffective(t, effectiveFixture{
		cpuTotal: i32(8), cpuAvailable: i32(4), // agent already accounted -4 cores
		memTotalMib: i64(16384), memAvailMib: i64(8192),
		lastHB: plus(1 * time.Hour), // heartbeat in the future vs VM creation
	})
	_ = pinVM(t, pinVMArgs{
		ownerID: ownerID, nodeID: nodeID,
		cpuCores: 4, memoryMib: 8192,
		createdAt: ago(2 * time.Hour), // VM existed before heartbeat
	})

	cpuEff, memEff := queryEffective(t, nodeID)
	// Agent's report already includes the VM (cpu_cores_available=4);
	// view should NOT subtract again. Effective = raw available.
	if cpuEff == nil || *cpuEff != 4 {
		t.Errorf("cpu_cores_effective = %v, want *int32(4) (no double-count)", deref32(cpuEff))
	}
	if memEff == nil || *memEff != 8192 {
		t.Errorf("memory_effective_mib = %v, want *int64(8192) (no double-count)", deref64(memEff))
	}
}

// TestEffectiveAvailability_NullHeartbeatSubtractsAll covers the
// bootstrap case: node row exists but never heartbeat'd
// (last_heartbeat_at IS NULL). The view treats this as "no agent
// observation has happened" и subtracts every pinned VM. cpu_cores_
// available is also nil (no heartbeat); effective stays nil (CASE).
func TestEffectiveAvailability_NullHeartbeatSubtractsAll(t *testing.T) {
	ownerID, nodeID := seedNodeForEffective(t, effectiveFixture{
		// No heartbeat columns set → effective is NULL by CASE.
	})
	_ = pinVM(t, pinVMArgs{
		ownerID: ownerID, nodeID: nodeID,
		cpuCores: 2, memoryMib: 4096,
	})
	cpuEff, memEff := queryEffective(t, nodeID)
	if cpuEff != nil {
		t.Errorf("cpu_cores_effective = %d, want nil (raw NULL → effective NULL)", *cpuEff)
	}
	if memEff != nil {
		t.Errorf("memory_effective_mib = %d, want nil (raw NULL → effective NULL)", *memEff)
	}
}

// TestEffectiveAvailability_NullHeartbeatWithMetricsSubtractsAll covers
// the unusual case where raw availability is set но last_heartbeat_at
// is NULL (synthetic state е.g., seeded directly without heartbeat).
// The view's LATERAL filter takes the `n.last_heartbeat_at is null OR
// vms.created_at > n.last_heartbeat_at` branch → all pinned VMs
// subtracted.
func TestEffectiveAvailability_NullHeartbeatWithMetricsSubtractsAll(t *testing.T) {
	ownerID, nodeID := seedNodeForEffective(t, effectiveFixture{
		cpuTotal: i32(8), cpuAvailable: i32(8),
		memTotalMib: i64(16384), memAvailMib: i64(16384),
		// lastHB unset (NULL).
	})
	_ = pinVM(t, pinVMArgs{
		ownerID: ownerID, nodeID: nodeID,
		cpuCores: 4, memoryMib: 8192,
	})
	cpuEff, memEff := queryEffective(t, nodeID)
	if cpuEff == nil || *cpuEff != 4 {
		t.Errorf("cpu_cores_effective = %v, want *int32(4)", deref32(cpuEff))
	}
	if memEff == nil || *memEff != 8192 {
		t.Errorf("memory_effective_mib = %v, want *int64(8192)", deref64(memEff))
	}
}

// TestEffectiveAvailability_FloorAtZero confirms the GREATEST(0, ...)
// guard: pending VMs totaling more than raw availability should not
// produce negative effective values. Operators see "0 free" instead.
func TestEffectiveAvailability_FloorAtZero(t *testing.T) {
	ownerID, nodeID := seedNodeForEffective(t, effectiveFixture{
		cpuTotal: i32(4), cpuAvailable: i32(2),
		memTotalMib: i64(4096), memAvailMib: i64(2048),
		lastHB: ago(1 * time.Hour),
	})
	// Pin а VM that wants more than the node can supply (post-pending).
	_ = pinVM(t, pinVMArgs{
		ownerID: ownerID, nodeID: nodeID,
		cpuCores: 16, memoryMib: 32768,
	})
	cpuEff, memEff := queryEffective(t, nodeID)
	if cpuEff == nil || *cpuEff != 0 {
		t.Errorf("cpu_cores_effective = %v, want *int32(0)", deref32(cpuEff))
	}
	if memEff == nil || *memEff != 0 {
		t.Errorf("memory_effective_mib = %v, want *int64(0)", deref64(memEff))
	}
}

// TestEffectiveAvailability_MultiplePendingStack confirms that
// multiple pending VMs accumulate inside the LATERAL aggregate —
// SUM() semantics, not single-row.
func TestEffectiveAvailability_MultiplePendingStack(t *testing.T) {
	ownerID, nodeID := seedNodeForEffective(t, effectiveFixture{
		cpuTotal: i32(16), cpuAvailable: i32(16),
		memTotalMib: i64(32768), memAvailMib: i64(32768),
		lastHB: ago(1 * time.Hour),
	})
	for _, spec := range []struct {
		cores int32
		mib   int32
	}{
		{cores: 2, mib: 4096},
		{cores: 4, mib: 8192},
		{cores: 1, mib: 2048},
	} {
		_ = pinVM(t, pinVMArgs{
			ownerID: ownerID, nodeID: nodeID,
			cpuCores: spec.cores, memoryMib: spec.mib,
		})
	}
	// Total pending: 7 cores, 14336 MiB.
	cpuEff, memEff := queryEffective(t, nodeID)
	if cpuEff == nil || *cpuEff != 9 {
		t.Errorf("cpu_cores_effective = %v, want *int32(9)", deref32(cpuEff))
	}
	if memEff == nil || *memEff != 18432 {
		t.Errorf("memory_effective_mib = %v, want *int64(18432)", deref64(memEff))
	}
}

// TestEffectiveAvailability_DesiredDeletedNotSubtracted confirms that
// VMs whose `desired_phase` is `'deleted'` (operator wants tear-down)
// do not contribute к the pending aggregate. They are en-route к
// removal; reserving capacity для them would block fresh placements
// for no purpose.
func TestEffectiveAvailability_DesiredDeletedNotSubtracted(t *testing.T) {
	ownerID, nodeID := seedNodeForEffective(t, effectiveFixture{
		cpuTotal: i32(8), cpuAvailable: i32(8),
		memTotalMib: i64(16384), memAvailMib: i64(16384),
		lastHB: ago(1 * time.Hour),
	})
	_ = pinVM(t, pinVMArgs{
		ownerID: ownerID, nodeID: nodeID,
		cpuCores: 4, memoryMib: 8192,
		desiredPhase: "deleted",
	})
	cpuEff, memEff := queryEffective(t, nodeID)
	if cpuEff == nil || *cpuEff != 8 {
		t.Errorf("cpu_cores_effective = %v, want *int32(8) (deleted not subtracted)", deref32(cpuEff))
	}
	if memEff == nil || *memEff != 16384 {
		t.Errorf("memory_effective_mib = %v, want *int64(16384) (deleted not subtracted)", deref64(memEff))
	}
}

// TestEffectiveAvailability_SoftDeletedNotSubtracted confirms that
// VMs с `deleted_at IS NOT NULL` (soft-deleted) drop out of the
// pending aggregate. Same intent as desired_phase='deleted' guard —
// don't reserve capacity for departed VMs.
func TestEffectiveAvailability_SoftDeletedNotSubtracted(t *testing.T) {
	ownerID, nodeID := seedNodeForEffective(t, effectiveFixture{
		cpuTotal: i32(8), cpuAvailable: i32(8),
		memTotalMib: i64(16384), memAvailMib: i64(16384),
		lastHB: ago(1 * time.Hour),
	})
	_ = pinVM(t, pinVMArgs{
		ownerID: ownerID, nodeID: nodeID,
		cpuCores: 4, memoryMib: 8192,
		deletedAt: ts(time.Now()),
	})
	cpuEff, _ := queryEffective(t, nodeID)
	if cpuEff == nil || *cpuEff != 8 {
		t.Errorf("cpu_cores_effective = %v, want *int32(8) (soft-deleted not subtracted)", deref32(cpuEff))
	}
}

// TestEffectiveAvailability_PinnedElsewhereNotSubtracted confirms the
// per-node scope of the LATERAL: а VM pinned к node-Y does not
// reduce node-X's effective availability.
func TestEffectiveAvailability_PinnedElsewhereNotSubtracted(t *testing.T) {
	_, nodeX := seedNodeForEffective(t, effectiveFixture{
		cpuTotal: i32(8), cpuAvailable: i32(8),
		memTotalMib: i64(16384), memAvailMib: i64(16384),
		lastHB: ago(1 * time.Hour),
	})
	ownerID, nodeY := seedNodeForEffective(t, effectiveFixture{
		cpuTotal: i32(8), cpuAvailable: i32(8),
		memTotalMib: i64(16384), memAvailMib: i64(16384),
		lastHB: ago(1 * time.Hour),
	})
	_ = pinVM(t, pinVMArgs{
		ownerID: ownerID, nodeID: nodeY,
		cpuCores: 4, memoryMib: 8192,
	})
	// nodeX's effective stays at raw (8 / 16384).
	cpuEffX, memEffX := queryEffective(t, nodeX)
	if cpuEffX == nil || *cpuEffX != 8 {
		t.Errorf("node-X cpu_effective = %v, want *int32(8) (VM pinned к node-Y, not subtracted)", deref32(cpuEffX))
	}
	if memEffX == nil || *memEffX != 16384 {
		t.Errorf("node-X mem_effective = %v, want *int64(16384)", deref64(memEffX))
	}
	// nodeY's effective reflects the pinned VM.
	cpuEffY, _ := queryEffective(t, nodeY)
	if cpuEffY == nil || *cpuEffY != 4 {
		t.Errorf("node-Y cpu_effective = %v, want *int32(4)", deref32(cpuEffY))
	}
}

// TestEffectiveAvailability_RaceReproduction is the end-to-end bug
// reproduction. Two placements happen back-to-back inside the
// heartbeat window: first VM commits, second VM's placement query
// must see effective availability reflecting the first. Without the
// view (or с naive raw availability), the second query reads stale
// "8 cores free" и a hypothetical second 6-core placement would
// over-allocate.
func TestEffectiveAvailability_RaceReproduction(t *testing.T) {
	ownerID, nodeID := seedNodeForEffective(t, effectiveFixture{
		cpuTotal: i32(8), cpuAvailable: i32(8),
		memTotalMib: i64(16384), memAvailMib: i64(16384),
		lastHB: ago(1 * time.Hour),
	})
	// First placement commits VM-A.
	_ = pinVM(t, pinVMArgs{
		ownerID: ownerID, nodeID: nodeID,
		cpuCores: 6, memoryMib: 12288,
	})
	// Second placement query reads the view BEFORE а new heartbeat
	// arrives. Without the fix, raw cpu_cores_available still says 8
	// (agent has not yet observed VM-A); the view's effective surfaces
	// the truth.
	cpuEff, memEff := queryEffective(t, nodeID)
	if cpuEff == nil || *cpuEff != 2 {
		t.Errorf("cpu_cores_effective = %v, want *int32(2) (race-window subtraction)", deref32(cpuEff))
	}
	if memEff == nil || *memEff != 4096 {
		t.Errorf("memory_effective_mib = %v, want *int64(4096)", deref64(memEff))
	}
}

// helpers — *int{32,64} → printable form для error messages.
func deref32(p *int32) any {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func deref64(p *int64) any {
	if p == nil {
		return "<nil>"
	}
	return *p
}
