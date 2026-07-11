// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// fakeQuerier is a hand-rolled stub implementing scheduler.Querier.
// Pool rows and per-node VM counts are programmed per case; the
// allPools slice backs the "pool exists anywhere" fallback path.
type fakeQuerier struct {
	eligible           []store.ListEligiblePoolsByNameRow
	pressuredMemory    []store.ListMemoryPressuredCandidatesByNameRow
	pressuredSysDisk   []store.ListSystemDiskPressuredCandidatesByNameRow
	pressuredPoolDisk  []store.ListDiskPressuredPoolsByNameRow
	allPools           []store.StoragePool
	vmCounts           map[uuid.UUID]int64
	networkStatus      map[uuid.UUID][]store.NetworkNodeStatus
	listErr            error
	listAllErr         error
	listMemPressureErr error
	listSysPressureErr error
	listDiskPoolErr    error
	listNetStatusErr   error
	countErr           error
	countErrFor        *uuid.UUID
}

func (f *fakeQuerier) ListEligiblePoolsByName(_ context.Context, _ string) ([]store.ListEligiblePoolsByNameRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.eligible, nil
}

func (f *fakeQuerier) ListMemoryPressuredCandidatesByName(_ context.Context, _ string) ([]store.ListMemoryPressuredCandidatesByNameRow, error) {
	if f.listMemPressureErr != nil {
		return nil, f.listMemPressureErr
	}
	return f.pressuredMemory, nil
}

func (f *fakeQuerier) ListSystemDiskPressuredCandidatesByName(_ context.Context, _ string) ([]store.ListSystemDiskPressuredCandidatesByNameRow, error) {
	if f.listSysPressureErr != nil {
		return nil, f.listSysPressureErr
	}
	return f.pressuredSysDisk, nil
}

func (f *fakeQuerier) ListDiskPressuredPoolsByName(_ context.Context, _ string) ([]store.ListDiskPressuredPoolsByNameRow, error) {
	if f.listDiskPoolErr != nil {
		return nil, f.listDiskPoolErr
	}
	return f.pressuredPoolDisk, nil
}

func (f *fakeQuerier) ListStoragePoolsByName(_ context.Context, _ string) ([]store.StoragePool, error) {
	if f.listAllErr != nil {
		return nil, f.listAllErr
	}
	return f.allPools, nil
}

func (f *fakeQuerier) ListNetworkNodeStatusByNode(_ context.Context, nodeID uuid.UUID) ([]store.NetworkNodeStatus, error) {
	if f.listNetStatusErr != nil {
		return nil, f.listNetStatusErr
	}
	return f.networkStatus[nodeID], nil
}

func (f *fakeQuerier) CountRunningVMsByNode(_ context.Context, nodeID *uuid.UUID) (int64, error) {
	if f.countErr != nil && (f.countErrFor == nil || (nodeID != nil && *f.countErrFor == *nodeID)) {
		return 0, f.countErr
	}
	if nodeID == nil {
		return 0, nil
	}
	return f.vmCounts[*nodeID], nil
}

// nodeFixture controls a fake (pool, node) row. Defaults yield a node
// without heartbeat metrics — set the metrics fields explicitly when a
// case needs resource-aware behaviour. nameOverride lets a case break
// the alphabetic alignment between UUID byte order and node name, which
// is how we exercise the name lexicographic tie-break independently of
// UUID ordering.
//
// cpuEffective / memEffectiveMib mirror the view-backed effective
// columns. When left nil, makeRow defaults them to the raw available
// values — the no-pending-VMs base case the unit tests use by
// default. A case can override either to simulate a pending-VM
// subtraction (raw available stays high, effective drops).
type nodeFixture struct {
	uuidSuffix              byte
	name                    string
	cpuTotal                *int32
	cpuAvailable            *int32
	cpuEffective            *int32
	memTotalMib             *int64
	memAvailMib             *int64
	memEffectiveMib         *int64
	poolName                string
	nameOverride            bool
	capacityBytes           *int64
	availableBytes          *int64
	availableBytesEffective *int64
	capabilities            []byte
}

func makeRow(f nodeFixture) store.ListEligiblePoolsByNameRow {
	var nodeID uuid.UUID
	nodeID[15] = f.uuidSuffix
	var poolID uuid.UUID
	poolID[15] = f.uuidSuffix
	poolID[14] = 0xAA
	name := f.name
	if name == "" && !f.nameOverride {
		// Default naming mirrors the trailing UUID byte: suffix 1 → "node-a", etc.
		name = "node-" + string([]byte{'a' + f.uuidSuffix - 1})
	}
	poolName := f.poolName
	if poolName == "" {
		poolName = "default"
	}
	// Default effective to raw available — most tests do not simulate
	// pending-VM subtraction and want fit check / scoring to behave as
	// "no pending". A case with pending sets cpuEffective / memEffective‑
	// Mib explicitly to a smaller value.
	cpuEff := f.cpuEffective
	if cpuEff == nil && f.cpuAvailable != nil {
		v := *f.cpuAvailable
		cpuEff = &v
	}
	memEff := f.memEffectiveMib
	if memEff == nil && f.memAvailMib != nil {
		v := *f.memAvailMib
		memEff = &v
	}
	// Pool effective column defaults to raw available — parallel to the
	// node CPU / memory effective fallback (no pending disks). Tests
	// with disk-aware coverage override either field. CapacityBytes is
	// left as f.capacityBytes verbatim — a nil capacity is a valid
	// pre-scan state.
	availEff := f.availableBytesEffective
	if availEff == nil && f.availableBytes != nil {
		v := *f.availableBytes
		availEff = &v
	}
	return store.ListEligiblePoolsByNameRow{
		NodeEffectiveAvailability: store.NodeEffectiveAvailability{
			ID:                 nodeID,
			Name:               name,
			CPUCoresTotal:      f.cpuTotal,
			CPUCoresAvailable:  f.cpuAvailable,
			CPUCoresEffective:  cpuEff,
			MemoryTotalMib:     f.memTotalMib,
			MemoryAvailableMib: f.memAvailMib,
			MemoryEffectiveMib: memEff,
			Capabilities:       f.capabilities,
		},
		PoolEffectiveCapacity: store.PoolEffectiveCapacity{
			ID:                      poolID,
			NodeID:                  nodeID,
			Name:                    poolName,
			Type:                    "local_dir",
			CapacityBytes:           f.capacityBytes,
			AvailableBytes:          f.availableBytes,
			AvailableBytesEffective: availEff,
		},
	}
}

// metrics is a convenience helper producing the four-pointer signature
// of a node with reported heartbeat data. The fit-check + scoring
// paths read cpu_cores_effective / memory_effective_mib; makeRow
// defaults those to raw available so this helper still covers the
// "no pending" case with four arguments.
func metrics(cpuTotal, cpuAvail int32, memTotalMib, memAvailMib int64) (*int32, *int32, *int64, *int64) {
	return &cpuTotal, &cpuAvail, &memTotalMib, &memAvailMib
}

// effective returns pointers for the effective-availability columns —
// used by tests that simulate a pending VM subtracting from raw
// availability. Pair with metrics() when building a fixture.
func effective(cpuEff int32, memEffMib int64) (*int32, *int64) {
	return &cpuEff, &memEffMib
}

// toStoragePool projects a PoolEffectiveCapacity row back onto the
// raw storage_pools shape — the same projection sqlc would produce
// from `ListStoragePoolsByName`. Used in tests that need to populate
// fakeQuerier.allPools (the existence-fallback path) from an `r*`
// fixture already built via makeRow.
func toStoragePool(r store.ListEligiblePoolsByNameRow) store.StoragePool {
	p := r.PoolEffectiveCapacity
	return store.StoragePool{
		ID:             p.ID,
		NodeID:         p.NodeID,
		Name:           p.Name,
		Type:           p.Type,
		Path:           p.Path,
		CapacityBytes:  p.CapacityBytes,
		AvailableBytes: p.AvailableBytes,
		ReportedAt:     p.ReportedAt,
		Config:         p.Config,
		CreatedAt:      p.CreatedAt,
		UpdatedAt:      p.UpdatedAt,
		DeletedAt:      p.DeletedAt,
	}
}

// defaultResources is the scheduler-internal default ResourcesConfig
// — every resource enabled, ratio 1.0. Mirror of
// internal/config::defaultResourcesConfig; kept here so test cases
// can build PlacementConfig values without crossing package boundaries.
// Sub-iteration C tests that exercise per-resource gating override
// individual fields on top of this value.
func defaultResources() ResourcesConfig {
	return ResourcesConfig{
		CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
		Memory: MemoryResourceConfig{Enabled: true, OvercommitRatio: 1.0},
		Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
	}
}

func TestSchedulePlacement(t *testing.T) {
	emptyPool := ""
	noSuchPool := "missing"
	hintNodeA := "node-a"
	hintNodeC := "node-c" // not in cluster

	r1 := makeRow(nodeFixture{uuidSuffix: 1})
	r2 := makeRow(nodeFixture{uuidSuffix: 2})
	r3 := makeRow(nodeFixture{uuidSuffix: 3})

	cases := []struct {
		name       string
		req        PlacementRequest
		cfg        PlacementConfig
		querier    *fakeQuerier
		wantErrIs  error
		wantNodeID uuid.UUID
	}{
		{
			name:      "empty pool name fails",
			req:       PlacementRequest{PoolName: emptyPool},
			querier:   &fakeQuerier{},
			wantErrIs: ErrPoolNotFound,
		},
		{
			name: "pool nowhere in cluster",
			req:  PlacementRequest{PoolName: noSuchPool},
			querier: &fakeQuerier{
				eligible: nil,
				allPools: nil,
			},
			wantErrIs: ErrPoolNotFound,
		},
		{
			name: "pool exists but every node cordoned",
			req:  PlacementRequest{PoolName: "default"},
			querier: &fakeQuerier{
				eligible: nil,
				allPools: []store.StoragePool{toStoragePool(r1), toStoragePool(r2)},
			},
			wantErrIs: ErrNoEligibleNodes,
		},
		{
			name: "single eligible node without metrics — count fallback selects it",
			req:  PlacementRequest{PoolName: "default", VCPUs: 4, MemoryMiB: 8192},
			querier: &fakeQuerier{
				eligible: []store.ListEligiblePoolsByNameRow{r1},
				vmCounts: map[uuid.UUID]int64{r1.NodeEffectiveAvailability.ID: 5},
			},
			wantNodeID: r1.NodeEffectiveAvailability.ID,
		},
		{
			name: "node hint matches by name (case-insensitive)",
			req:  PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512, NodeHint: stringPtr("NODE-A")},
			querier: &fakeQuerier{
				eligible: []store.ListEligiblePoolsByNameRow{r1, r2, r3},
				vmCounts: map[uuid.UUID]int64{
					r1.NodeEffectiveAvailability.ID: 50,
					r2.NodeEffectiveAvailability.ID: 1,
					r3.NodeEffectiveAvailability.ID: 1,
				},
			},
			wantNodeID: r1.NodeEffectiveAvailability.ID,
		},
		{
			name: "node hint rejects uuid literal",
			req:  PlacementRequest{PoolName: "default", NodeHint: stringPtr(r2.NodeEffectiveAvailability.ID.String())},
			querier: &fakeQuerier{
				eligible: []store.ListEligiblePoolsByNameRow{r1, r2, r3},
			},
			wantErrIs: ErrNodeHintIsUUID,
		},
		{
			name: "node hint references node that does not host pool",
			req:  PlacementRequest{PoolName: "default", NodeHint: &hintNodeC},
			querier: &fakeQuerier{
				eligible: []store.ListEligiblePoolsByNameRow{r1, r2},
			},
			wantErrIs: ErrPoolNotOnNode,
		},
		{
			name: "node hint references cordoned node (not in eligible list)",
			req:  PlacementRequest{PoolName: "default", NodeHint: &hintNodeA},
			querier: &fakeQuerier{
				eligible: []store.ListEligiblePoolsByNameRow{r2, r3},
				// node-a (r1) is intentionally absent — simulates cordon.
			},
			wantErrIs: ErrPoolNotOnNode,
		},
		{
			name: "underlying count query failure surfaces (fallback path)",
			req:  PlacementRequest{PoolName: "default"},
			querier: &fakeQuerier{
				eligible: []store.ListEligiblePoolsByNameRow{r1, r2},
				countErr: errors.New("pgx: dial tcp: connection refused"),
			},
			wantErrIs: nil, // asserted via Contains below
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Table-test cases predate Sub-iteration C — their cfg
			// literals do not set Resources. A zero-value
			// ResourcesConfig has every resource disabled, which
			// would skip the fit check entirely. Substitute the
			// strict default so each case exercises the same gates
			// it did before With-iteration.
			cfg := tc.cfg
			if cfg.Resources == (ResourcesConfig{}) {
				cfg.Resources = defaultResources()
			}
			got, err := SchedulePlacement(context.Background(), tc.querier, tc.req, cfg)
			if tc.wantErrIs != nil {
				if err == nil {
					t.Fatalf("SchedulePlacement(%v) = no error, want %v", tc.req, tc.wantErrIs)
				}
				if !errors.Is(err, tc.wantErrIs) {
					t.Errorf("SchedulePlacement(%v) err = %v, want errors.Is %v", tc.req, err, tc.wantErrIs)
				}
				return
			}
			if tc.querier.countErr != nil {
				if err == nil {
					t.Fatalf("SchedulePlacement(%v) = no error, want non-nil from count failure", tc.req)
				}
				if !strings.Contains(err.Error(), "count running vms") {
					t.Errorf("SchedulePlacement count failure err = %q, want chain to mention 'count running vms'", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SchedulePlacement(%v) err = %v, want nil", tc.req, err)
			}
			if got.Node.ID != tc.wantNodeID {
				t.Errorf("SchedulePlacement(%v) node = %s, want %s", tc.req, got.Node.ID, tc.wantNodeID)
			}
			if got.PoolInstance.NodeID != tc.wantNodeID {
				t.Errorf("SchedulePlacement(%v) pool.NodeID = %s, want %s", tc.req, got.PoolInstance.NodeID, tc.wantNodeID)
			}
		})
	}
}

func TestSchedulePlacement_ListErrorSurfaced(t *testing.T) {
	q := &fakeQuerier{listErr: errors.New("pgx: query failed")}
	_, err := SchedulePlacement(context.Background(), q, PlacementRequest{PoolName: "default"}, PlacementConfig{})
	if err == nil {
		t.Fatal("SchedulePlacement = no error, want non-nil")
	}
	if !strings.Contains(err.Error(), "list eligible pools") {
		t.Errorf("err = %q, want chain to mention 'list eligible pools'", err)
	}
}

// TestSchedulePlacement_ResourceAware exercises the new LeastAllocated
// path: nodes report metrics, fit check excludes the under-resourced,
// scoring picks the least-utilized post-placement.
func TestSchedulePlacement_ResourceAware(t *testing.T) {
	// node-busy: 8 cores total, 1 effective (87.5% used).
	// node-spare: 8 cores total, 6 effective (25% used).
	// Both fit the 1-core request; node-spare must win on score.
	busyCPUTotal, busyCPUAvail, busyMemTotal, busyMemAvail := metrics(8, 1, 16384, 2048)
	spareCPUTotal, spareCPUAvail, spareMemTotal, spareMemAvail := metrics(8, 6, 16384, 12288)
	busy := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-busy", nameOverride: true,
		cpuTotal: busyCPUTotal, cpuAvailable: busyCPUAvail,
		memTotalMib: busyMemTotal, memAvailMib: busyMemAvail,
	})
	spare := makeRow(nodeFixture{
		uuidSuffix: 2, name: "node-spare", nameOverride: true,
		cpuTotal: spareCPUTotal, cpuAvailable: spareCPUAvail,
		memTotalMib: spareMemTotal, memAvailMib: spareMemAvail,
	})

	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{busy, spare}}
	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("SchedulePlacement err = %v, want nil", err)
	}
	if got.Node.ID != spare.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-spare (%s)", got.Node.Name, spare.NodeEffectiveAvailability.Name)
	}
}

// TestSchedulePlacement_EffectiveDrivesFit confirms the fit check uses
// the effective columns, not the raw heartbeat columns: a node that
// reports 8 free cores via heartbeat but carries 4 cores of pending
// VMs (effective=4) correctly rejects an 8-core request.
func TestSchedulePlacement_EffectiveDrivesFit(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 8, 16384, 16384)
	// Simulate a pending 4-vCPU / 8 GiB VM subtracting from raw.
	cpuEff, memEff := effective(4, 8192)
	row := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail, cpuEffective: cpuEff,
		memTotalMib: memTotal, memAvailMib: memAvail, memEffectiveMib: memEff,
	})
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{row}}

	// 8-vCPU request fits raw availability (8 ≤ 8) but not effective (8 > 4).
	_, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 8, MemoryMiB: 4096},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if !errors.Is(err, ErrInsufficientResources) {
		t.Fatalf("err = %v, want errors.Is ErrInsufficientResources (fit should use effective, not raw)", err)
	}

	// 4-vCPU request fits effective (4 ≤ 4) and lands.
	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 4, MemoryMiB: 4096},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("4-vCPU request err = %v, want nil", err)
	}
	if got.Node.ID != row.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-a", got.Node.Name)
	}
}

// TestSchedulePlacement_InsufficientCPU verifies the fit check rejects
// a node that cannot accommodate the requested CPU, and the resulting
// error carries a structured payload.
func TestSchedulePlacement_InsufficientCPU(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(4, 1, 16384, 14336)
	row := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{row}}

	_, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 8, MemoryMiB: 4096},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if !errors.Is(err, ErrInsufficientResources) {
		t.Fatalf("err = %v, want errors.Is ErrInsufficientResources", err)
	}
	detail, ok := ExtractInsufficientResources(err)
	if !ok || detail == nil {
		t.Fatalf("ExtractInsufficientResources(_) = (%v, %v), want detail present", detail, ok)
	}
	if detail.RequiredCPU != 8 || detail.RequiredMemMiB != 4096 {
		t.Errorf("required = (cpu=%d, mem=%d), want (8, 4096)", detail.RequiredCPU, detail.RequiredMemMiB)
	}
	if detail.CandidatesConsidered != 1 || detail.CandidatesEligible != 0 {
		t.Errorf("counts = (considered=%d, eligible=%d), want (1, 0)", detail.CandidatesConsidered, detail.CandidatesEligible)
	}
	// renderUtilization computes used as `total - effective`; here effective
	// defaults to raw available (1) so used = 4 - 1 = 3 (matches old raw
	// computation since the fixture has no pending VMs).
	wantUtil := []NodeUtilization{
		{Node: "node-a", Pool: "default", CPUUsed: 3, CPUTotal: 4, MemUsedMiB: 2048, MemTotalMiB: 16384},
	}
	if diff := cmp.Diff(wantUtil, detail.NodeUtilization); diff != "" {
		t.Errorf("NodeUtilization mismatch (-want +got):\n%s", diff)
	}
}

// TestSchedulePlacement_InsufficientCPU_ReportsEffective verifies that
// when a pending VM brings effective below raw, the utilization
// payload reflects the effective (post-pending) accounting — operators
// see the scheduler's actual view, not the agent's stale snapshot.
func TestSchedulePlacement_InsufficientCPU_ReportsEffective(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 8, 16384, 16384)
	cpuEff, memEff := effective(2, 4096)
	row := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail, cpuEffective: cpuEff,
		memTotalMib: memTotal, memAvailMib: memAvail, memEffectiveMib: memEff,
	})
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{row}}

	_, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 4, MemoryMiB: 8192},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if !errors.Is(err, ErrInsufficientResources) {
		t.Fatalf("err = %v, want errors.Is ErrInsufficientResources", err)
	}
	detail, _ := ExtractInsufficientResources(err)
	// total - effective = 8 - 2 = 6 cores "used" per effective view.
	wantUtil := []NodeUtilization{
		{Node: "node-a", Pool: "default", CPUUsed: 6, CPUTotal: 8, MemUsedMiB: 12288, MemTotalMiB: 16384},
	}
	if diff := cmp.Diff(wantUtil, detail.NodeUtilization); diff != "" {
		t.Errorf("NodeUtilization mismatch (-want +got):\n%s", diff)
	}
}

// TestSchedulePlacement_InsufficientMemory verifies memory-only
// rejection (CPU fits but memory does not).
func TestSchedulePlacement_InsufficientMemory(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 6, 4096, 1024)
	row := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{row}}

	_, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 2, MemoryMiB: 2048},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if !errors.Is(err, ErrInsufficientResources) {
		t.Fatalf("err = %v, want errors.Is ErrInsufficientResources", err)
	}
}

// TestSchedulePlacement_TieBreakByNameNotUUID is the core consistency
// test for the algorithm change: when two candidates score equally,
// the **name lexicographic** (lowercase) tie-break decides, not UUID
// byte order. The fixture deliberately sets up a contradiction —
// node-z carries the smallest UUID byte, node-a the largest — so the
// old UUID byte-order tie-break would pick node-z; the new name
// tie-break correctly picks node-a.
func TestSchedulePlacement_TieBreakByNameNotUUID(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 4, 16384, 8192)
	cpuTotalB, cpuAvailB, memTotalB, memAvailB := metrics(8, 4, 16384, 8192)
	cpuTotalC, cpuAvailC, memTotalC, memAvailC := metrics(8, 4, 16384, 8192)
	// UUID byte order: nodeZ (suffix 1) < nodeM (suffix 2) < nodeA (suffix 3).
	// Lowercase name order: node-a < node-m < node-z. The two orderings
	// are reversed, so a passing test confirms the name tie-break wins.
	nodeZ := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-z", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	nodeM := makeRow(nodeFixture{
		uuidSuffix: 2, name: "node-m", nameOverride: true,
		cpuTotal: cpuTotalB, cpuAvailable: cpuAvailB,
		memTotalMib: memTotalB, memAvailMib: memAvailB,
	})
	nodeA := makeRow(nodeFixture{
		uuidSuffix: 3, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotalC, cpuAvailable: cpuAvailC,
		memTotalMib: memTotalC, memAvailMib: memAvailC,
	})
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{nodeZ, nodeM, nodeA}}

	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("SchedulePlacement err = %v, want nil", err)
	}
	if got.Node.ID != nodeA.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-a (the lexicographically smallest name)", got.Node.Name)
	}
}

// TestSchedulePlacement_TieBreakCaseInsensitive guards the lowercase
// comparison contract — names that differ only in case order
// consistently regardless of input casing.
func TestSchedulePlacement_TieBreakCaseInsensitive(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 4, 16384, 8192)
	cpuTotal2, cpuAvail2, memTotal2, memAvail2 := metrics(8, 4, 16384, 8192)
	upper := makeRow(nodeFixture{
		uuidSuffix: 1, name: "NODE-A", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	lower := makeRow(nodeFixture{
		uuidSuffix: 2, name: "node-b", nameOverride: true,
		cpuTotal: cpuTotal2, cpuAvailable: cpuAvail2,
		memTotalMib: memTotal2, memAvailMib: memAvail2,
	})
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{upper, lower}}

	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("SchedulePlacement err = %v, want nil", err)
	}
	// lowercase("NODE-A") = "node-a", lowercase("node-b") = "node-b"; node-a wins.
	if got.Node.ID != upper.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want NODE-A", got.Node.Name)
	}
}

// TestSchedulePlacement_MixedMetrics confirms that a cluster where some
// nodes report heartbeat metrics and others have not yet keeps both
// classes eligible, and that a metric-bearing winner outranks a
// fallback candidate of comparable apparent load.
func TestSchedulePlacement_MixedMetrics(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 6, 16384, 12288)
	withMetrics := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	withoutMetrics := makeRow(nodeFixture{
		uuidSuffix: 2, name: "node-b", nameOverride: true,
	})

	q := &fakeQuerier{
		eligible: []store.ListEligiblePoolsByNameRow{withMetrics, withoutMetrics},
		vmCounts: map[uuid.UUID]int64{withoutMetrics.NodeEffectiveAvailability.ID: 0},
	}
	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("SchedulePlacement err = %v, want nil", err)
	}
	// The fallback score lives at 1.0+count = 1.0; the metric-bearing
	// node scores around (2/8 + 4/16)/2 ≈ 0.25, well below 1.0.
	if got.Node.ID != withMetrics.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want metric-bearing node-a", got.Node.Name)
	}
}

// TestSchedulePlacement_AllWithoutMetrics confirms graceful degradation
// to pure count-based scoring when no node has heartbeated yet
// (cluster bootstrap). Lower count wins, name tie-break still applies.
func TestSchedulePlacement_AllWithoutMetrics(t *testing.T) {
	a := makeRow(nodeFixture{uuidSuffix: 1, name: "node-a", nameOverride: true})
	b := makeRow(nodeFixture{uuidSuffix: 2, name: "node-b", nameOverride: true})
	q := &fakeQuerier{
		eligible: []store.ListEligiblePoolsByNameRow{a, b},
		vmCounts: map[uuid.UUID]int64{a.NodeEffectiveAvailability.ID: 5, b.NodeEffectiveAvailability.ID: 2},
	}
	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Node.ID != b.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-b (lower count)", got.Node.Name)
	}
}

// TestSchedulePlacement_LeastVMCountAlgorithm exercises the count-based
// algorithm path. With identical heartbeat metrics, the candidate with
// fewer pinned VMs wins — a pure-count decision unaffected by
// resource utilization.
func TestSchedulePlacement_LeastVMCountAlgorithm(t *testing.T) {
	// Both nodes equally utilized but node-b carries fewer VMs.
	cpuTotalA, cpuAvailA, memTotalA, memAvailA := metrics(8, 4, 16384, 8192)
	cpuTotalB, cpuAvailB, memTotalB, memAvailB := metrics(8, 4, 16384, 8192)
	a := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotalA, cpuAvailable: cpuAvailA,
		memTotalMib: memTotalA, memAvailMib: memAvailA,
	})
	b := makeRow(nodeFixture{
		uuidSuffix: 2, name: "node-b", nameOverride: true,
		cpuTotal: cpuTotalB, cpuAvailable: cpuAvailB,
		memTotalMib: memTotalB, memAvailMib: memAvailB,
	})
	q := &fakeQuerier{
		eligible: []store.ListEligiblePoolsByNameRow{a, b},
		vmCounts: map[uuid.UUID]int64{a.NodeEffectiveAvailability.ID: 10, b.NodeEffectiveAvailability.ID: 3},
	}
	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512},
		PlacementConfig{Algorithm: AlgorithmLeastVMCount, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Node.ID != b.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-b (fewer VMs)", got.Node.Name)
	}
}

// TestSchedulePlacement_LeastVMCountSharesTieBreak confirms that both
// algorithm paths use the same name lexicographic tie-break — a
// least_vm_count tie resolves by name, not by UUID byte order.
func TestSchedulePlacement_LeastVMCountSharesTieBreak(t *testing.T) {
	// Identical metrics, identical VM counts → tie → name decides.
	cpuTotalA, cpuAvailA, memTotalA, memAvailA := metrics(8, 4, 16384, 8192)
	cpuTotalZ, cpuAvailZ, memTotalZ, memAvailZ := metrics(8, 4, 16384, 8192)
	// node-z carries a smaller UUID (suffix 1), node-a carries a bigger
	// UUID (suffix 9) — UUID byte order would pick node-z.
	z := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-z", nameOverride: true,
		cpuTotal: cpuTotalZ, cpuAvailable: cpuAvailZ,
		memTotalMib: memTotalZ, memAvailMib: memAvailZ,
	})
	a := makeRow(nodeFixture{
		uuidSuffix: 9, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotalA, cpuAvailable: cpuAvailA,
		memTotalMib: memTotalA, memAvailMib: memAvailA,
	})
	q := &fakeQuerier{
		eligible: []store.ListEligiblePoolsByNameRow{z, a},
		vmCounts: map[uuid.UUID]int64{z.NodeEffectiveAvailability.ID: 5, a.NodeEffectiveAvailability.ID: 5},
	}
	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512},
		PlacementConfig{Algorithm: AlgorithmLeastVMCount, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Node.ID != a.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-a (name tie-break)", got.Node.Name)
	}
}

// TestSchedulePlacement_UnknownAlgorithm guards the defensive surface:
// if an unknown algorithm reaches the hot path despite config
// validation, the scheduler returns the typed sentinel rather than
// silently picking a default.
func TestSchedulePlacement_UnknownAlgorithm(t *testing.T) {
	row := makeRow(nodeFixture{uuidSuffix: 1})
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{row}}
	_, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512},
		PlacementConfig{Algorithm: "nonsense"})
	if !errors.Is(err, ErrUnknownAlgorithm) {
		t.Errorf("err = %v, want errors.Is ErrUnknownAlgorithm", err)
	}
}

// TestSchedulePlacement_NodeHintInsufficientFits surfaces the case
// where the operator pins a hint to a specific node and that node
// cannot fit the request — the resulting envelope carries the
// insufficient-resources detail (not pool_not_on_node), since the
// pool IS hosted on the node.
func TestSchedulePlacement_NodeHintInsufficientFits(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(4, 1, 8192, 1024)
	row := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{row}}
	hint := "node-a"
	_, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 8, MemoryMiB: 4096, NodeHint: &hint},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if !errors.Is(err, ErrInsufficientResources) {
		t.Fatalf("err = %v, want errors.Is ErrInsufficientResources", err)
	}
}

func stringPtr(s string) *string { return &s }

// TestSchedulePlacement_PressureDetailWhenAllFiltered exercises the
// memory-pressure failure path: the eligible list is empty (every
// candidate filtered out) and the diagnostic query reports nodes under
// memory pressure. The error wraps ErrNoEligibleNodes and carries a
// NodePressureDetail accessible via ExtractNodePressureDetail.
func TestSchedulePlacement_PressureDetailWhenAllFiltered(t *testing.T) {
	r1 := makeRow(nodeFixture{uuidSuffix: 1, name: "node-a", nameOverride: true})
	since := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	r1.NodeEffectiveAvailability.MemoryPressureSince = &since
	r1.NodeEffectiveAvailability.MemoryPressureCount = 3

	pressuredRow := store.ListMemoryPressuredCandidatesByNameRow(r1)

	q := &fakeQuerier{
		// eligible empty — SQL filter excludes pressured rows.
		eligible:        nil,
		allPools:        []store.StoragePool{toStoragePool(r1)},
		pressuredMemory: []store.ListMemoryPressuredCandidatesByNameRow{pressuredRow},
	}

	_, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if !errors.Is(err, ErrNoEligibleNodes) {
		t.Fatalf("err = %v, want errors.Is ErrNoEligibleNodes", err)
	}
	detail, ok := ExtractNodePressureDetail(err)
	if !ok || detail == nil {
		t.Fatalf("ExtractNodePressureDetail returned ok=%v detail=%v, want pressure detail", ok, detail)
	}
	if len(detail.Nodes) != 1 {
		t.Fatalf("len(Nodes) = %d, want 1", len(detail.Nodes))
	}
	got := detail.Nodes[0]
	if got.Node != "node-a" {
		t.Errorf("Node = %q, want %q", got.Node, "node-a")
	}
	if len(got.Conditions) != 1 || got.Conditions[0] != "memory_pressure" {
		t.Errorf("Conditions = %v, want [memory_pressure]", got.Conditions)
	}
	if got.MemoryPressureSince == nil || !got.MemoryPressureSince.Equal(since) {
		t.Errorf("MemoryPressureSince = %v, want %v", got.MemoryPressureSince, since)
	}
}

// TestSchedulePlacement_NoPressureWhenAllCordoned confirms the bare
// ErrNoEligibleNodes path stays untouched when the diagnostic query
// returns no pressured candidates — pool exists, all cordoned, no
// pressure → no NodePressureDetail attached.
func TestSchedulePlacement_NoPressureWhenAllCordoned(t *testing.T) {
	r1 := makeRow(nodeFixture{uuidSuffix: 1, name: "node-a", nameOverride: true})
	q := &fakeQuerier{
		eligible: nil,
		allPools: []store.StoragePool{toStoragePool(r1)},
		// no pressure-set candidates of any type
	}
	_, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if !errors.Is(err, ErrNoEligibleNodes) {
		t.Fatalf("err = %v, want errors.Is ErrNoEligibleNodes", err)
	}
	if _, ok := ExtractNodePressureDetail(err); ok {
		t.Errorf("ExtractNodePressureDetail = ok, want false (no pressured rows)")
	}
}

// TestSchedulePlacement_AllThreePressureTypesCombined exercises the
// merge logic in buildNodePressureDetail: memory pressure on node-a,
// system_disk pressure on node-b, and pool disk pressure on node-c's
// pool. Result: three entries in the detail, each carrying the
// appropriate condition flag.
func TestSchedulePlacement_AllThreePressureTypesCombined(t *testing.T) {
	memTs := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	sysTs := time.Date(2026, 5, 12, 12, 5, 0, 0, time.UTC)
	diskTs := time.Date(2026, 5, 12, 12, 10, 0, 0, time.UTC)

	rA := makeRow(nodeFixture{uuidSuffix: 1, name: "node-a", nameOverride: true})
	rA.NodeEffectiveAvailability.MemoryPressureSince = &memTs

	rB := makeRow(nodeFixture{uuidSuffix: 2, name: "node-b", nameOverride: true})
	rB.NodeEffectiveAvailability.SystemDiskPressureSince = &sysTs

	rC := makeRow(nodeFixture{uuidSuffix: 3, name: "node-c", nameOverride: true})
	rC.PoolEffectiveCapacity.DiskPressureSince = &diskTs

	q := &fakeQuerier{
		eligible:          nil,
		allPools:          []store.StoragePool{toStoragePool(rA)},
		pressuredMemory:   []store.ListMemoryPressuredCandidatesByNameRow{store.ListMemoryPressuredCandidatesByNameRow(rA)},
		pressuredSysDisk:  []store.ListSystemDiskPressuredCandidatesByNameRow{store.ListSystemDiskPressuredCandidatesByNameRow(rB)},
		pressuredPoolDisk: []store.ListDiskPressuredPoolsByNameRow{store.ListDiskPressuredPoolsByNameRow(rC)},
	}

	_, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if !errors.Is(err, ErrNoEligibleNodes) {
		t.Fatalf("err = %v, want errors.Is ErrNoEligibleNodes", err)
	}
	detail, ok := ExtractNodePressureDetail(err)
	if !ok || detail == nil {
		t.Fatalf("ExtractNodePressureDetail = ok=%v detail=%v", ok, detail)
	}
	if got, want := len(detail.Nodes), 3; got != want {
		t.Fatalf("len(Nodes) = %d, want %d (%v)", got, want, detail.Nodes)
	}

	// node-a: memory_pressure
	if detail.Nodes[0].Node != "node-a" {
		t.Errorf("Nodes[0].Node = %q, want node-a", detail.Nodes[0].Node)
	}
	if detail.Nodes[0].Pool != "" {
		t.Errorf("Nodes[0].Pool = %q, want empty (node-scoped)", detail.Nodes[0].Pool)
	}
	if len(detail.Nodes[0].Conditions) != 1 || detail.Nodes[0].Conditions[0] != "memory_pressure" {
		t.Errorf("Nodes[0].Conditions = %v, want [memory_pressure]", detail.Nodes[0].Conditions)
	}

	// node-b: system_disk_pressure
	if detail.Nodes[1].Node != "node-b" {
		t.Errorf("Nodes[1].Node = %q, want node-b", detail.Nodes[1].Node)
	}
	if len(detail.Nodes[1].Conditions) != 1 || detail.Nodes[1].Conditions[0] != "system_disk_pressure" {
		t.Errorf("Nodes[1].Conditions = %v, want [system_disk_pressure]", detail.Nodes[1].Conditions)
	}

	// node-c: pool-scoped disk_pressure
	if detail.Nodes[2].Node != "node-c" {
		t.Errorf("Nodes[2].Node = %q, want node-c", detail.Nodes[2].Node)
	}
	if detail.Nodes[2].Pool != "default" {
		t.Errorf("Nodes[2].Pool = %q, want default", detail.Nodes[2].Pool)
	}
	if len(detail.Nodes[2].Conditions) != 1 || detail.Nodes[2].Conditions[0] != "disk_pressure" {
		t.Errorf("Nodes[2].Conditions = %v, want [disk_pressure]", detail.Nodes[2].Conditions)
	}
}

// TestSchedulePlacement_NodeWithBothMemoryAndSystemDiskPressure
// exercises the dedup logic in buildNodePressureDetail: both node-
// scoped queries return the same node, and the result merges into a
// single entry with two conditions[].
func TestSchedulePlacement_NodeWithBothMemoryAndSystemDiskPressure(t *testing.T) {
	memTs := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	sysTs := time.Date(2026, 5, 12, 12, 5, 0, 0, time.UTC)

	r := makeRow(nodeFixture{uuidSuffix: 1, name: "node-a", nameOverride: true})
	r.NodeEffectiveAvailability.MemoryPressureSince = &memTs
	r.NodeEffectiveAvailability.SystemDiskPressureSince = &sysTs

	q := &fakeQuerier{
		eligible:         nil,
		allPools:         []store.StoragePool{toStoragePool(r)},
		pressuredMemory:  []store.ListMemoryPressuredCandidatesByNameRow{store.ListMemoryPressuredCandidatesByNameRow(r)},
		pressuredSysDisk: []store.ListSystemDiskPressuredCandidatesByNameRow{store.ListSystemDiskPressuredCandidatesByNameRow(r)},
	}

	_, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if !errors.Is(err, ErrNoEligibleNodes) {
		t.Fatalf("err = %v, want ErrNoEligibleNodes", err)
	}
	detail, ok := ExtractNodePressureDetail(err)
	if !ok || detail == nil {
		t.Fatalf("ExtractNodePressureDetail returned %v / %v", ok, detail)
	}
	if got, want := len(detail.Nodes), 1; got != want {
		t.Fatalf("len(Nodes) = %d, want %d (entries: %v)", got, want, detail.Nodes)
	}
	entry := detail.Nodes[0]
	if entry.Node != "node-a" {
		t.Errorf("Node = %q, want node-a", entry.Node)
	}
	if len(entry.Conditions) != 2 {
		t.Errorf("Conditions = %v, want [memory_pressure system_disk_pressure]", entry.Conditions)
	}
	if entry.MemoryPressureSince == nil || !entry.MemoryPressureSince.Equal(memTs) {
		t.Errorf("MemoryPressureSince = %v, want %v", entry.MemoryPressureSince, memTs)
	}
	if entry.SystemDiskPressureSince == nil || !entry.SystemDiskPressureSince.Equal(sysTs) {
		t.Errorf("SystemDiskPressureSince = %v, want %v", entry.SystemDiskPressureSince, sysTs)
	}
}

// ----- Sub-iteration C: disk-aware fit + per-resource gate -----
//
// Helpers below build rows with disk capacity / availability populated.
// Existing tests default disk fields to nil, which makes
// candidateHasMetrics fall back to count-based for disk-enabled
// configs; the With-aware cases pin them explicitly.

// diskRow wraps makeRow with disk capacity + availability set to the
// supplied bytes. AvailableBytesEffective defaults to raw available
// (no pending disks); cases with pending pressure pass an explicit
// override.
func diskRow(f nodeFixture, capacity, available int64) store.ListEligiblePoolsByNameRow {
	f.capacityBytes = i64Ptr(capacity)
	f.availableBytes = i64Ptr(available)
	return makeRow(f)
}

func i64Ptr(v int64) *int64 { return &v }

// TestSchedulePlacement_DiskFit_Sufficient — disk-aware enabled, ample
// pool capacity → placement succeeds.
func TestSchedulePlacement_DiskFit_Sufficient(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 8, 16384, 16384)
	row := diskRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	}, 200*1073741824, 200*1073741824)
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{row}}

	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 4, MemoryMiB: 8192, DiskBytes: 20 * 1073741824},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("err = %v, want nil (sufficient disk)", err)
	}
	if got.Node.ID != row.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-a", got.Node.Name)
	}
}

// TestSchedulePlacement_DiskFit_Insufficient — disk-aware enabled,
// pool too tight → ErrInsufficientResources with disk fields populated.
func TestSchedulePlacement_DiskFit_Insufficient(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 8, 16384, 16384)
	row := diskRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
		poolName: "pool-tight",
	}, 50*1073741824, 5*1073741824)
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{row}}

	_, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 4, MemoryMiB: 8192, DiskBytes: 20 * 1073741824},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if !errors.Is(err, ErrInsufficientResources) {
		t.Fatalf("err = %v, want errors.Is ErrInsufficientResources (insufficient disk)", err)
	}
	detail, ok := ExtractInsufficientResources(err)
	if !ok || detail == nil {
		t.Fatalf("ExtractInsufficientResources(%v) = no detail", err)
	}
	if detail.RequiredDiskBytes != 20*1073741824 {
		t.Errorf("RequiredDiskBytes = %d, want %d", detail.RequiredDiskBytes, 20*1073741824)
	}
	if len(detail.NodeUtilization) != 1 {
		t.Fatalf("NodeUtilization len = %d, want 1", len(detail.NodeUtilization))
	}
	u := detail.NodeUtilization[0]
	if u.Pool != "pool-tight" {
		t.Errorf("Pool = %q, want pool-tight", u.Pool)
	}
	if u.DiskTotalBytes != 50*1073741824 || u.DiskUsedBytes != 45*1073741824 {
		t.Errorf("disk = used=%d/total=%d, want used=45GiB/total=50GiB",
			u.DiskUsedBytes, u.DiskTotalBytes)
	}
}

// TestSchedulePlacement_DiskDisabled_BypassesCheck — cfg.Disk.Enabled=false
// → tight pool still fits because disk does not participate.
func TestSchedulePlacement_DiskDisabled_BypassesCheck(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 8, 16384, 16384)
	row := diskRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	}, 50*1073741824, 1*1073741824)
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{row}}

	cfg := defaultResources()
	cfg.Disk.Enabled = false
	_, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 4, MemoryMiB: 8192, DiskBytes: 20 * 1073741824},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: cfg})
	if err != nil {
		t.Errorf("err = %v, want nil (disk disabled bypasses fit)", err)
	}
}

// TestSchedulePlacement_CPUDisabled_TwoFactorScore — cfg.CPU.Enabled=false,
// memory + disk score the candidates. Highest-disk-avail wins.
func TestSchedulePlacement_CPUDisabled_TwoFactorScore(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 8, 16384, 16384)
	tight := diskRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
		poolName: "p1",
	}, 100*1073741824, 30*1073741824) // 70% disk used
	loose := diskRow(nodeFixture{
		uuidSuffix: 2, name: "node-b", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
		poolName: "p2",
	}, 100*1073741824, 90*1073741824) // 10% disk used
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{tight, loose}}

	cfg := defaultResources()
	cfg.CPU.Enabled = false
	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 4, MemoryMiB: 8192, DiskBytes: 10 * 1073741824},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: cfg})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	// loose pool ranks lower on disk axis → wins despite equal memory util.
	if got.Node.ID != loose.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-b (loose pool)", got.Node.Name)
	}
}

// TestSchedulePlacement_AllDisabled_CountFallback — every resource
// off → score derived from CountRunningVMsByNode (Least-VM-count
// parity without switching the cluster algorithm).
func TestSchedulePlacement_AllDisabled_CountFallback(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 8, 16384, 16384)
	a := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	b := makeRow(nodeFixture{
		uuidSuffix: 2, name: "node-b", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	q := &fakeQuerier{
		eligible: []store.ListEligiblePoolsByNameRow{a, b},
		vmCounts: map[uuid.UUID]int64{
			a.NodeEffectiveAvailability.ID: 50,
			b.NodeEffectiveAvailability.ID: 1,
		},
	}

	cfg := ResourcesConfig{} // every Enabled=false
	// Validation requires positive ratios so set them; Enabled remains false.
	cfg.CPU.OvercommitRatio = 1.0
	cfg.Memory.OvercommitRatio = 1.0
	cfg.Disk.OvercommitRatio = 1.0
	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 4, MemoryMiB: 8192},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: cfg})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got.Node.ID != b.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-b (lowest VM count)", got.Node.Name)
	}
}

// TestSchedulePlacement_OvercommitInflatesCapacity — ratio > 1.0 on a
// node with a zram net allows a fit that strict 1.0 would reject.
// Asserts the zram-bounded headroom reaches the fit check.
func TestSchedulePlacement_OvercommitInflatesCapacity(t *testing.T) {
	// Memory is the tight resource: 8192 MiB total, 2048 MiB effective,
	// request 3000 MiB. Strict 1.0 rejects (2048 < 3000); ratio 2.0 with a
	// 2048 MiB zram net adds 2048 MiB headroom (avail 4096) and admits.
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 8, 8192, 2048)
	row := diskRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
		capabilities: []byte(`{"compressed_swap":{"size_mib":2048}}`),
	}, 200*1073741824, 200*1073741824)
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{row}}

	// Strict: rejected.
	strict := defaultResources()
	_, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 3000, DiskBytes: 1073741824},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: strict})
	if !errors.Is(err, ErrInsufficientResources) {
		t.Errorf("strict err = %v, want ErrInsufficientResources", err)
	}

	// 2.0 overcommit on memory, bounded by the zram net: admitted.
	loose := defaultResources()
	loose.Memory.OvercommitRatio = 2.0
	loose.Memory.OvercommitZramFloorMib = 256
	loose.Memory.OvercommitZramConfidence = 1.0
	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 3000, DiskBytes: 1073741824},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: loose})
	if err != nil {
		t.Fatalf("overcommit err = %v, want nil (ratio 2.0 inflates capacity)", err)
	}
	if got.Node.ID != row.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-a", got.Node.Name)
	}
}

// TestSchedulePlacement_ThreeFactorScoring — three pools same
// CPU / memory load, different disk → choice per disk util.
func TestSchedulePlacement_ThreeFactorScoring(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 4, 8192, 4096)
	heavy := diskRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
		poolName: "p1",
	}, 100*1073741824, 20*1073741824) // 80% disk used
	medium := diskRow(nodeFixture{
		uuidSuffix: 2, name: "node-b", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
		poolName: "p2",
	}, 100*1073741824, 50*1073741824) // 50% disk used
	light := diskRow(nodeFixture{
		uuidSuffix: 3, name: "node-c", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
		poolName: "p3",
	}, 100*1073741824, 90*1073741824) // 10% disk used
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{heavy, medium, light}}

	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512, DiskBytes: 5 * 1073741824},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	// CPU / memory utilization identical across candidates, disk tips
	// the score — lightest pool wins.
	if got.Node.ID != light.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-c (lightest disk)", got.Node.Name)
	}
}

// TestSchedulePlacement_PoolWithoutMetrics_FallsBackToCount — pool
// row missing capacity / available (pre-scan) and disk-aware enabled →
// candidate falls into count-based scoring rather than rejection.
// Parallel to the node-without-heartbeat fallback for CPU/memory.
func TestSchedulePlacement_PoolWithoutMetrics_FallsBackToCount(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 8, 16384, 16384)
	// Two candidates: A has pool metrics (low load); B has none. Disk-
	// aware should rank A ahead because B uses count fallback.
	a := diskRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	}, 100*1073741824, 80*1073741824)
	b := makeRow(nodeFixture{ // No capacity_bytes / available_bytes — pool unscanned.
		uuidSuffix: 2, name: "node-b", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	q := &fakeQuerier{
		eligible: []store.ListEligiblePoolsByNameRow{a, b},
		vmCounts: map[uuid.UUID]int64{b.NodeEffectiveAvailability.ID: 0},
	}

	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512, DiskBytes: 5 * 1073741824},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	// A wins — resource-aware scoring lives in [0, 1]; B's fallback
	// score sits at 1.0+0 = 1.0, so A (small disk util) outranks.
	if got.Node.ID != a.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-a (metric-bearing outranks fallback)", got.Node.Name)
	}
}

// ----- network-aware placement -----

// TestSchedulePlacement_NetworkFailedExcludesNode confirms the network
// filter excludes a node where a requested network failed to reconcile.
// Two ready nodes both fit the request and host the pool; the requested
// network reconciled `ready` on node-a and `failed` on node-b. Placement
// must pick node-a and never node-b.
func TestSchedulePlacement_NetworkFailedExcludesNode(t *testing.T) {
	netX := uuid.MustParse("00000000-0000-0000-0000-0000000000a0")

	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 8, 16384, 16384)
	a := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	b := makeRow(nodeFixture{
		uuidSuffix: 2, name: "node-b", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	q := &fakeQuerier{
		eligible: []store.ListEligiblePoolsByNameRow{a, b},
		networkStatus: map[uuid.UUID][]store.NetworkNodeStatus{
			a.NodeEffectiveAvailability.ID: {{NetworkID: netX, NodeID: a.NodeEffectiveAvailability.ID, ReconciliationStatus: "ready"}},
			b.NodeEffectiveAvailability.ID: {{NetworkID: netX, NodeID: b.NodeEffectiveAvailability.ID, ReconciliationStatus: "failed"}},
		},
	}

	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512, NetworkIDs: []uuid.UUID{netX}},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("SchedulePlacement err = %v, want nil", err)
	}
	if got.Node.ID != a.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-a (network ready); node-b had network failed", got.Node.Name)
	}
}

// TestSchedulePlacement_NetworkFailedEverywhere confirms that when the
// requested network failed to reconcile on every candidate node,
// placement returns ErrNoEligibleNodes and the structured detail names
// the network and the failed reason.
func TestSchedulePlacement_NetworkFailedEverywhere(t *testing.T) {
	netX := uuid.MustParse("00000000-0000-0000-0000-0000000000a0")

	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 8, 16384, 16384)
	a := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	b := makeRow(nodeFixture{
		uuidSuffix: 2, name: "node-b", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	q := &fakeQuerier{
		eligible: []store.ListEligiblePoolsByNameRow{a, b},
		networkStatus: map[uuid.UUID][]store.NetworkNodeStatus{
			a.NodeEffectiveAvailability.ID: {{NetworkID: netX, NodeID: a.NodeEffectiveAvailability.ID, ReconciliationStatus: "failed"}},
			b.NodeEffectiveAvailability.ID: {{NetworkID: netX, NodeID: b.NodeEffectiveAvailability.ID, ReconciliationStatus: "failed"}},
		},
	}

	_, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512, NetworkIDs: []uuid.UUID{netX}},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if !errors.Is(err, ErrNoEligibleNodes) {
		t.Fatalf("err = %v, want errors.Is ErrNoEligibleNodes", err)
	}
	detail, ok := ExtractNetworkUnreadyDetail(err)
	if !ok || detail == nil {
		t.Fatalf("ExtractNetworkUnreadyDetail = ok=%v detail=%v, want network-unready detail", ok, detail)
	}
	if len(detail.Nodes) != 2 {
		t.Fatalf("len(Nodes) = %d, want 2 (%v)", len(detail.Nodes), detail.Nodes)
	}
	first := detail.Nodes[0]
	if first.Node != "node-a" {
		t.Errorf("Nodes[0].Node = %q, want node-a", first.Node)
	}
	if len(first.Networks) != 1 || first.Networks[0].NetworkID != netX {
		t.Errorf("Nodes[0].Networks = %v, want [%s]", first.Networks, netX)
	}
	if first.Networks[0].Status != "failed" {
		t.Errorf("Nodes[0].Networks[0].Status = %q, want failed", first.Networks[0].Status)
	}
}

// TestSchedulePlacement_NetworkMissingExcludesNode confirms a node with
// no per-(node, network) status record for the requested network (the
// agent never reported the network at all) is excluded — absence is
// treated as not-ready, never as a silent pass.
func TestSchedulePlacement_NetworkMissingExcludesNode(t *testing.T) {
	netX := uuid.MustParse("00000000-0000-0000-0000-0000000000a0")

	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 8, 16384, 16384)
	a := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	b := makeRow(nodeFixture{
		uuidSuffix: 2, name: "node-b", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	q := &fakeQuerier{
		eligible: []store.ListEligiblePoolsByNameRow{a, b},
		networkStatus: map[uuid.UUID][]store.NetworkNodeStatus{
			// node-a has the network ready; node-b reports nothing.
			a.NodeEffectiveAvailability.ID: {{NetworkID: netX, NodeID: a.NodeEffectiveAvailability.ID, ReconciliationStatus: "ready"}},
		},
	}

	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512, NetworkIDs: []uuid.UUID{netX}},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("SchedulePlacement err = %v, want nil", err)
	}
	if got.Node.ID != a.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-a (node-b missing network status)", got.Node.Name)
	}
}

// TestSchedulePlacement_NoNetworkRequested confirms the filter is a
// no-op when the request names no networks — the network-status query is
// never consulted and placement behaves exactly as before.
func TestSchedulePlacement_NoNetworkRequested(t *testing.T) {
	cpuTotal, cpuAvail, memTotal, memAvail := metrics(8, 8, 16384, 16384)
	a := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotal, cpuAvailable: cpuAvail,
		memTotalMib: memTotal, memAvailMib: memAvail,
	})
	// listNetStatusErr would surface if the filter consulted the query;
	// a no-op filter must never reach it.
	q := &fakeQuerier{
		eligible:         []store.ListEligiblePoolsByNameRow{a},
		listNetStatusErr: errors.New("network status query must not run"),
	}

	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("SchedulePlacement err = %v, want nil", err)
	}
	if got.Node.ID != a.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-a", got.Node.Name)
	}
}

// ----- exclude-current-node placement (migration) -----

// TestSchedulePlacement_ExcludesCurrentNode confirms ExcludeNodeID drops
// the current node from the eligible set so a node-less migrate never
// re-pins onto the source. Two equally-fitting nodes A and B; excluding A
// makes B the only survivor, regardless of any scoring nondeterminism.
func TestSchedulePlacement_ExcludesCurrentNode(t *testing.T) {
	cpuTotalA, cpuAvailA, memTotalA, memAvailA := metrics(8, 8, 16384, 16384)
	cpuTotalB, cpuAvailB, memTotalB, memAvailB := metrics(8, 8, 16384, 16384)
	a := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-a", nameOverride: true,
		cpuTotal: cpuTotalA, cpuAvailable: cpuAvailA,
		memTotalMib: memTotalA, memAvailMib: memAvailA,
	})
	b := makeRow(nodeFixture{
		uuidSuffix: 2, name: "node-b", nameOverride: true,
		cpuTotal: cpuTotalB, cpuAvailable: cpuAvailB,
		memTotalMib: memTotalB, memAvailMib: memAvailB,
	})
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{a, b}}
	excludeA := a.NodeEffectiveAvailability.ID

	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512, ExcludeNodeID: &excludeA},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("SchedulePlacement err = %v, want nil", err)
	}
	if got.Node.ID != b.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-b (node-a excluded)", got.Node.Name)
	}
}

// TestSchedulePlacement_ExcludeOnlyNodeEmptyEligible confirms that
// excluding the sole eligible node empties the candidate set and routes
// through the same empty-eligible diagnosis path the zero-candidates case
// uses — a retryable ErrNoEligibleNodes, not a panic.
func TestSchedulePlacement_ExcludeOnlyNodeEmptyEligible(t *testing.T) {
	a := makeRow(nodeFixture{uuidSuffix: 1, name: "node-a", nameOverride: true})
	q := &fakeQuerier{
		eligible: []store.ListEligiblePoolsByNameRow{a},
		allPools: []store.StoragePool{toStoragePool(a)},
	}
	excludeA := a.NodeEffectiveAvailability.ID

	_, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512, ExcludeNodeID: &excludeA},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if !errors.Is(err, ErrNoEligibleNodes) {
		t.Fatalf("err = %v, want errors.Is ErrNoEligibleNodes", err)
	}
}

// TestSchedulePlacement_NoExcludeUnchanged confirms a nil ExcludeNodeID is
// a no-op: the resource-aware happy path still picks the least-utilized
// node exactly as it did before exclusion existed.
func TestSchedulePlacement_NoExcludeUnchanged(t *testing.T) {
	busyCPUTotal, busyCPUAvail, busyMemTotal, busyMemAvail := metrics(8, 1, 16384, 2048)
	spareCPUTotal, spareCPUAvail, spareMemTotal, spareMemAvail := metrics(8, 6, 16384, 12288)
	busy := makeRow(nodeFixture{
		uuidSuffix: 1, name: "node-busy", nameOverride: true,
		cpuTotal: busyCPUTotal, cpuAvailable: busyCPUAvail,
		memTotalMib: busyMemTotal, memAvailMib: busyMemAvail,
	})
	spare := makeRow(nodeFixture{
		uuidSuffix: 2, name: "node-spare", nameOverride: true,
		cpuTotal: spareCPUTotal, cpuAvailable: spareCPUAvail,
		memTotalMib: spareMemTotal, memAvailMib: spareMemAvail,
	})
	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{busy, spare}}

	got, err := SchedulePlacement(context.Background(), q,
		PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 512},
		PlacementConfig{Algorithm: AlgorithmResourceAware, Resources: defaultResources()})
	if err != nil {
		t.Fatalf("SchedulePlacement err = %v, want nil", err)
	}
	if got.Node.ID != spare.NodeEffectiveAvailability.ID {
		t.Errorf("winner = %s, want node-spare (nil exclude must be a no-op)", got.Node.Name)
	}
}

func TestParseCompressedSwapSizeMib(t *testing.T) {
	tests := []struct {
		name     string
		caps     string
		wantSize int64
		wantOK   bool
	}{
		{name: "present", caps: `{"compressed_swap":{"size_mib":2048,"algorithm":"zstd"}}`, wantSize: 2048, wantOK: true},
		{name: "absent field", caps: `{"cpu_model":"x"}`, wantSize: 0, wantOK: false},
		{name: "empty", caps: ``, wantSize: 0, wantOK: false},
		{name: "malformed", caps: `{not json`, wantSize: 0, wantOK: false},
		{name: "null compressed_swap", caps: `{"compressed_swap":null}`, wantSize: 0, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseCompressedSwapSizeMib([]byte(tt.caps))
			if got != tt.wantSize || ok != tt.wantOK {
				t.Errorf("parseCompressedSwapSizeMib(%q) = (%d, %v), want (%d, %v)", tt.caps, got, ok, tt.wantSize, tt.wantOK)
			}
		})
	}
}

func TestMemOvercommitHeadroom(t *testing.T) {
	total := func(v int64) *int64 { return &v }
	caps := func(sizeMib int64) []byte {
		return []byte(fmt.Sprintf(`{"compressed_swap":{"size_mib":%d}}`, sizeMib))
	}
	tests := []struct {
		name string
		node store.NodeEffectiveAvailability
		cfg  MemoryResourceConfig
		want int64
	}{
		{
			name: "ratio 1.0 is a no-op",
			node: store.NodeEffectiveAvailability{MemoryTotalMib: total(8192), Capabilities: caps(2048)},
			cfg:  MemoryResourceConfig{Enabled: true, OvercommitRatio: 1.0, OvercommitZramFloorMib: 256, OvercommitZramConfidence: 1.0},
			want: 0,
		},
		{
			name: "no zram device is strict",
			node: store.NodeEffectiveAvailability{MemoryTotalMib: total(8192), Capabilities: []byte(`{}`)},
			cfg:  MemoryResourceConfig{Enabled: true, OvercommitRatio: 2.0, OvercommitZramFloorMib: 256, OvercommitZramConfidence: 1.0},
			want: 0,
		},
		{
			name: "zram below floor is strict",
			node: store.NodeEffectiveAvailability{MemoryTotalMib: total(8192), Capabilities: caps(100)},
			cfg:  MemoryResourceConfig{Enabled: true, OvercommitRatio: 2.0, OvercommitZramFloorMib: 256, OvercommitZramConfidence: 1.0},
			want: 0,
		},
		{
			name: "zram ceiling binds (ratio 2.0, zram=ram/4 -> 1.25x)",
			node: store.NodeEffectiveAvailability{MemoryTotalMib: total(8192), Capabilities: caps(2048)},
			cfg:  MemoryResourceConfig{Enabled: true, OvercommitRatio: 2.0, OvercommitZramFloorMib: 256, OvercommitZramConfidence: 1.0},
			want: 2048, // min(8192*(2-1)=8192, 2048*1.0=2048)
		},
		{
			name: "operator ceiling binds (ratio 1.1)",
			node: store.NodeEffectiveAvailability{MemoryTotalMib: total(8192), Capabilities: caps(4096)},
			cfg:  MemoryResourceConfig{Enabled: true, OvercommitRatio: 1.1, OvercommitZramFloorMib: 256, OvercommitZramConfidence: 1.0},
			want: 819, // min(8192*0.1=819.2 -> 819, 4096*1.0=4096)
		},
		{
			name: "confidence discount",
			node: store.NodeEffectiveAvailability{MemoryTotalMib: total(8192), Capabilities: caps(2048)},
			cfg:  MemoryResourceConfig{Enabled: true, OvercommitRatio: 2.0, OvercommitZramFloorMib: 256, OvercommitZramConfidence: 0.9},
			want: 1843, // min(8192, 2048*0.9=1843.2 -> 1843)
		},
		{
			name: "nil total is fail-closed",
			node: store.NodeEffectiveAvailability{MemoryTotalMib: nil, Capabilities: caps(2048)},
			cfg:  MemoryResourceConfig{Enabled: true, OvercommitRatio: 2.0, OvercommitZramFloorMib: 256, OvercommitZramConfidence: 1.0},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MemOvercommitHeadroom(tt.node, tt.cfg); got != tt.want {
				t.Errorf("MemOvercommitHeadroom(%s) = %d, want %d", tt.name, got, tt.want)
			}
		})
	}
}

func TestMemoryOvercommitAdmitsOnlyZramNode(t *testing.T) {
	// need 2560 MiB; node has 2048 MiB effective (strict reject) but 8192 total.
	// With ratio 2.0 + zram 2048: headroom 2048 -> avail 4096 >= 2560 -> admit.
	// The strict node sorts ahead of the zram node by name, so under the old
	// blind multiplier (2048*2.0=4096 >= 2560) it would wrongly win.
	zCPUTotal, zCPUAvail, zMemTotal, zMemAvail := metrics(8, 8, 8192, 2048)
	withZram := makeRow(nodeFixture{
		uuidSuffix: 1, name: "zram-node",
		cpuTotal: zCPUTotal, cpuAvailable: zCPUAvail,
		memTotalMib: zMemTotal, memAvailMib: zMemAvail,
		capabilities: []byte(`{"compressed_swap":{"size_mib":2048}}`),
	})
	sCPUTotal, sCPUAvail, sMemTotal, sMemAvail := metrics(8, 8, 8192, 2048)
	noZram := makeRow(nodeFixture{
		uuidSuffix: 2, name: "strict-node",
		cpuTotal: sCPUTotal, cpuAvailable: sCPUAvail,
		memTotalMib: sMemTotal, memAvailMib: sMemAvail,
		capabilities: []byte(`{}`),
	})

	cfg := PlacementConfig{
		Algorithm: AlgorithmResourceAware,
		Resources: ResourcesConfig{
			CPU:  ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
			Disk: ResourceConfig{Enabled: false},
			Memory: MemoryResourceConfig{
				Enabled: true, OvercommitRatio: 2.0,
				OvercommitZramFloorMib: 256, OvercommitZramConfidence: 1.0,
			},
		},
	}
	req := PlacementRequest{PoolName: "default", VCPUs: 1, MemoryMiB: 2560}

	q := &fakeQuerier{eligible: []store.ListEligiblePoolsByNameRow{withZram, noZram}}
	dec, err := SchedulePlacement(context.Background(), q, req, cfg)
	if err != nil {
		t.Fatalf("SchedulePlacement: %v", err)
	}
	if dec.Node.Name != "zram-node" {
		t.Errorf("winner = %q, want zram-node (only the zram node has headroom for 2560 MiB)", dec.Node.Name)
	}
}
