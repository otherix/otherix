// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/agent/zram"
	"github.com/otherix/otherix/internal/config"
)

type stubLister struct{ vms []*vm.VM }

func (s stubLister) List() []*vm.VM { return s.vms }

// TestLinuxCollector_ReadsProcCpuinfoAndMeminfo lays a synthetic
// /proc tree in a tempdir and confirms the collector parses the
// canonical Linux file formats correctly. We do not exercise the
// real /proc here — the syscall integration is covered by the
// runtime test below, and the parsers are the brittle parts worth
// pinning.
func TestLinuxCollector_ReadsProcCpuinfoAndMeminfo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(syntheticCPUInfo), 0o644); err != nil {
		t.Fatalf("write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(syntheticMemInfo), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}

	c := &LinuxCollector{
		procPath: dir,
		vms:      stubLister{},
	}
	model, flags, cores := c.readCPUInfo()
	if model != "AMD EPYC 9554" {
		t.Errorf("model = %q, want %q", model, "AMD EPYC 9554")
	}
	if cores != 4 {
		t.Errorf("cores = %d, want 4", cores)
	}
	if len(flags) == 0 || flags[0] != "fpu" {
		t.Errorf("flags[0] = %q, want fpu", flags[0])
	}

	mib := c.readMemTotalMib()
	if mib != 16384 {
		t.Errorf("memory_total_mib = %d, want 16384", mib)
	}
}

// TestLinuxCollector_FreeResourceSubtraction confirms the
// running-VM allocation arithmetic. Non-running VMs are excluded;
// negative values clamp to zero.
func TestLinuxCollector_FreeResourceSubtraction(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(syntheticCPUInfo), 0o644); err != nil {
		t.Fatalf("write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(syntheticMemInfo), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	vms := []*vm.VM{
		{ID: uuid.New(), Status: vm.StatusRunning, VCPUs: 2, MemoryMib: 2048},
		{ID: uuid.New(), Status: vm.StatusRunning, VCPUs: 1, MemoryMib: 1024},
		{ID: uuid.New(), Status: vm.StatusStopped, VCPUs: 99, MemoryMib: 99999}, // ignored
	}
	c := &LinuxCollector{
		procPath:     dir,
		vms:          stubLister{vms: vms},
		agentVersion: "test",
		architecture: "amd64",
	}

	rep, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got, want := rep.Resources.CPUCoresAvailable, int32(4-3); got != want {
		t.Errorf("cpu_cores_available = %d, want %d", got, want)
	}
	if got, want := rep.Resources.MemoryAvailableMib, int64(16384-3072); got != want {
		t.Errorf("memory_available_mib = %d, want %d", got, want)
	}
	if got, want := len(rep.VMs), 3; got != want {
		// Stopped VM still surfaces in the heartbeat (with phase=stopped) —
		// CP-side reconciler reads inventory from full snapshots, not
		// only running rows.
		t.Errorf("vms count = %d, want %d", got, want)
	}
}

type stubPoolReporter struct{ reports []PoolReport }

func (s stubPoolReporter) PoolReports() []PoolReport { return s.reports }

type fakePoolImageLister struct {
	images map[string][]PoolImageReport
	// unavailable pools report known=false (the agent could not enumerate them).
	unavailable map[string]bool
}

func (f fakePoolImageLister) PoolImages(pool string) ([]PoolImageReport, bool) {
	if f.unavailable[pool] {
		return nil, false
	}
	return f.images[pool], true
}

// TestCollectPoolImages confirms the collector folds the per-pool
// image inventory (from the PoolImageLister seam) into the matching
// PoolReport.Images for the heartbeat.
func TestCollectPoolImages(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(syntheticCPUInfo), 0o644); err != nil {
		t.Fatalf("write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(syntheticMemInfo), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	img := PoolImageReport{
		Basename:   "noble.qcow2",
		SHA256:     "abc123",
		SizeBytes:  1024,
		Format:     "qcow2",
		ImportedAt: "2026-06-05T00:00:00Z",
	}
	c := &LinuxCollector{
		procPath:     dir,
		vms:          stubLister{},
		agentVersion: "test",
		architecture: "amd64",
		pools:        stubPoolReporter{reports: []PoolReport{{Name: "default", ReconciliationStatus: "ready"}}},
		poolImages:   fakePoolImageLister{images: map[string][]PoolImageReport{"default": {img}}},
	}

	rep, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := len(rep.Pools); got != 1 {
		t.Fatalf("pools count = %d, want 1", got)
	}
	if diff := cmp.Diff([]PoolImageReport{img}, rep.Pools[0].Images); diff != "" {
		t.Errorf("Pools[0].Images mismatch (-want +got):\n%s", diff)
	}
	// A successful enumeration sets ImagesUnavailable = false; the inventory is
	// authoritative.
	if rep.Pools[0].ImagesUnavailable {
		t.Errorf("Pools[0].ImagesUnavailable = true, want false (enumeration succeeded)")
	}
}

// TestCollectPoolImagesUnavailable confirms the tri-state fail-closed contract:
// a PoolImageLister that returns known=false (the agent could not enumerate the
// pool) makes the collector set ImagesUnavailable = true with no images, so the
// CP preserves the prior inventory rather than clearing it.
func TestCollectPoolImagesUnavailable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(syntheticCPUInfo), 0o644); err != nil {
		t.Fatalf("write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(syntheticMemInfo), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	c := &LinuxCollector{
		procPath:     dir,
		vms:          stubLister{},
		agentVersion: "test",
		architecture: "amd64",
		pools:        stubPoolReporter{reports: []PoolReport{{Name: "default", ReconciliationStatus: "ready"}}},
		poolImages:   fakePoolImageLister{unavailable: map[string]bool{"default": true}},
	}

	rep, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := len(rep.Pools); got != 1 {
		t.Fatalf("pools count = %d, want 1", got)
	}
	if !rep.Pools[0].ImagesUnavailable {
		t.Errorf("Pools[0].ImagesUnavailable = false, want true (enumeration failed)")
	}
	if len(rep.Pools[0].Images) != 0 {
		t.Errorf("Pools[0].Images = %+v, want empty when unavailable", rep.Pools[0].Images)
	}
}

type fakePoolSnapshotLister struct {
	snapshots map[string][]PoolSnapshotReport
	// unavailable pools report known=false (the agent could not enumerate them).
	unavailable map[string]bool
}

func (f fakePoolSnapshotLister) PoolSnapshots(pool string) ([]PoolSnapshotReport, bool) {
	if f.unavailable[pool] {
		return nil, false
	}
	return f.snapshots[pool], true
}

// TestCollect_IncludesPoolSnapshots confirms the collector folds the per-pool
// snapshot-blob inventory (from the PoolSnapshotLister seam) into the matching
// PoolReport.Snapshots for the heartbeat. Mirrors TestCollectPoolImages.
func TestCollect_IncludesPoolSnapshots(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(syntheticCPUInfo), 0o644); err != nil {
		t.Fatalf("write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(syntheticMemInfo), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	snap := PoolSnapshotReport{
		SHA256:    "abc123",
		SizeBytes: 2048,
	}
	c := &LinuxCollector{
		procPath:      dir,
		vms:           stubLister{},
		agentVersion:  "test",
		architecture:  "amd64",
		pools:         stubPoolReporter{reports: []PoolReport{{Name: "default", ReconciliationStatus: "ready"}}},
		poolSnapshots: fakePoolSnapshotLister{snapshots: map[string][]PoolSnapshotReport{"default": {snap}}},
	}

	rep, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := len(rep.Pools); got != 1 {
		t.Fatalf("pools count = %d, want 1", got)
	}
	if diff := cmp.Diff([]PoolSnapshotReport{snap}, rep.Pools[0].Snapshots); diff != "" {
		t.Errorf("Pools[0].Snapshots mismatch (-want +got):\n%s", diff)
	}
	// A successful enumeration sets SnapshotsUnavailable = false; the inventory is
	// authoritative.
	if rep.Pools[0].SnapshotsUnavailable {
		t.Errorf("Pools[0].SnapshotsUnavailable = true, want false (enumeration succeeded)")
	}
}

// TestCollect_PoolSnapshotsUnavailable confirms the tri-state fail-closed
// contract: a PoolSnapshotLister that returns known=false (the agent could not
// enumerate the pool) makes the collector set SnapshotsUnavailable = true with no
// snapshots, so the CP preserves the prior inventory rather than clearing it.
// Mirrors TestCollectPoolImagesUnavailable.
func TestCollect_PoolSnapshotsUnavailable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(syntheticCPUInfo), 0o644); err != nil {
		t.Fatalf("write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(syntheticMemInfo), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	c := &LinuxCollector{
		procPath:      dir,
		vms:           stubLister{},
		agentVersion:  "test",
		architecture:  "amd64",
		pools:         stubPoolReporter{reports: []PoolReport{{Name: "default", ReconciliationStatus: "ready"}}},
		poolSnapshots: fakePoolSnapshotLister{unavailable: map[string]bool{"default": true}},
	}

	rep, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := len(rep.Pools); got != 1 {
		t.Fatalf("pools count = %d, want 1", got)
	}
	if !rep.Pools[0].SnapshotsUnavailable {
		t.Errorf("Pools[0].SnapshotsUnavailable = false, want true (enumeration failed)")
	}
	if len(rep.Pools[0].Snapshots) != 0 {
		t.Errorf("Pools[0].Snapshots = %+v, want empty when unavailable", rep.Pools[0].Snapshots)
	}
}

type fakeBlobLister struct {
	blobs []BlobReport
	// unavailable reports known=false (the agent could not enumerate the store).
	unavailable bool
}

func (f fakeBlobLister) NodeBlobs() ([]BlobReport, bool) {
	if f.unavailable {
		return nil, false
	}
	return f.blobs, true
}

// TestCollect_IncludesBlobs confirms the collector folds the node-level
// artifact-store blob inventory (from the BlobLister seam) into
// Report.Blobs for the heartbeat. Reported once per node, not per pool;
// mirrors TestCollect_IncludesPoolSnapshots.
func TestCollect_IncludesBlobs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(syntheticCPUInfo), 0o644); err != nil {
		t.Fatalf("write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(syntheticMemInfo), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	blob := BlobReport{
		Digest:    "abc123",
		SizeBytes: 4096,
	}
	c := &LinuxCollector{
		procPath:     dir,
		vms:          stubLister{},
		agentVersion: "test",
		architecture: "amd64",
		blobs:        fakeBlobLister{blobs: []BlobReport{blob}},
	}

	rep, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if diff := cmp.Diff([]BlobReport{blob}, rep.Blobs); diff != "" {
		t.Errorf("Blobs mismatch (-want +got):\n%s", diff)
	}
	// A successful enumeration sets BlobsUnavailable = false; the inventory is
	// authoritative.
	if rep.BlobsUnavailable {
		t.Errorf("BlobsUnavailable = true, want false (enumeration succeeded)")
	}
}

// TestCollect_BlobsUnavailable confirms the fail-closed contract: a
// BlobLister that returns known=false (the agent could not enumerate the
// artifact store) makes the collector set BlobsUnavailable = true with no
// blobs, so the CP preserves the prior inventory rather than clearing it.
// Mirrors TestCollect_PoolSnapshotsUnavailable.
func TestCollect_BlobsUnavailable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(syntheticCPUInfo), 0o644); err != nil {
		t.Fatalf("write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(syntheticMemInfo), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	c := &LinuxCollector{
		procPath:     dir,
		vms:          stubLister{},
		agentVersion: "test",
		architecture: "amd64",
		blobs:        fakeBlobLister{unavailable: true},
	}

	rep, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !rep.BlobsUnavailable {
		t.Errorf("BlobsUnavailable = false, want true (enumeration failed)")
	}
	if len(rep.Blobs) != 0 {
		t.Errorf("Blobs = %+v, want empty when unavailable", rep.Blobs)
	}
}

// TestCollectorFoldsImageBlobs confirms the collector folds the node-level
// image cache tier inventory (from the ImageBlobs seam) into Report.ImageBlobs.
// Reported once per node through a distinct field from Report.Blobs; mirrors
// TestCollect_IncludesBlobs.
func TestCollectorFoldsImageBlobs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(syntheticCPUInfo), 0o644); err != nil {
		t.Fatalf("write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(syntheticMemInfo), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	blob := BlobReport{
		Digest:    "img123",
		SizeBytes: 8192,
	}
	c := &LinuxCollector{
		procPath:     dir,
		vms:          stubLister{},
		agentVersion: "test",
		architecture: "amd64",
		imageBlobs:   fakeBlobLister{blobs: []BlobReport{blob}},
	}

	rep, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if diff := cmp.Diff([]BlobReport{blob}, rep.ImageBlobs); diff != "" {
		t.Errorf("ImageBlobs mismatch (-want +got):\n%s", diff)
	}
	if rep.ImageBlobsUnavailable {
		t.Errorf("ImageBlobsUnavailable = true, want false (enumeration succeeded)")
	}
}

// TestCollectorImageBlobsUnavailable confirms the fail-closed contract for the
// image cache tier: an ImageBlobs seam that returns known=false makes the
// collector set ImageBlobsUnavailable = true with no blobs, so the CP preserves
// the prior image_blobs inventory. Mirrors TestCollect_BlobsUnavailable.
func TestCollectorImageBlobsUnavailable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(syntheticCPUInfo), 0o644); err != nil {
		t.Fatalf("write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(syntheticMemInfo), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	c := &LinuxCollector{
		procPath:     dir,
		vms:          stubLister{},
		agentVersion: "test",
		architecture: "amd64",
		imageBlobs:   fakeBlobLister{unavailable: true},
	}

	rep, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !rep.ImageBlobsUnavailable {
		t.Errorf("ImageBlobsUnavailable = false, want true (enumeration failed)")
	}
	if len(rep.ImageBlobs) != 0 {
		t.Errorf("ImageBlobs = %+v, want empty when unavailable", rep.ImageBlobs)
	}
}

// TestMapVMStatus pins the agent→server phase mapping. The CP-side
// validator rejects anything outside the supported VmPhase enum, so
// drift here surfaces as a run-time 400.
func TestMapVMStatus(t *testing.T) {
	cases := map[vm.Status]string{
		vm.StatusPending:  "pending",
		vm.StatusCreating: "pending",
		vm.StatusRunning:  "running",
		vm.StatusStopping: "stopped",
		vm.StatusStopped:  "stopped",
		vm.StatusFailed:   "error",
		vm.StatusDeleting: "",
	}
	for in, want := range cases {
		if got := mapVMStatus(in); got != want {
			t.Errorf("mapVMStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestArchFromGo pins GOARCH translation.
func TestArchFromGo(t *testing.T) {
	cases := map[string]string{
		"amd64": "amd64",
		"arm64": "arm64",
		"386":   "",
		"":      "",
	}
	for in, want := range cases {
		if got := archFromGo(in); got != want {
			t.Errorf("archFromGo(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestClampNonNegative confirms underflow handling — sum of
// running VMs may exceed total reported cores/memory if the
// host's accounting drifts. Negative values surface as 0 rather
// than wrap-around to a huge positive number.
func TestClampNonNegative(t *testing.T) {
	if got := clampNonNegative32(-5); got != 0 {
		t.Errorf("clampNonNegative32(-5) = %d, want 0", got)
	}
	if got := clampNonNegative32(7); got != 7 {
		t.Errorf("clampNonNegative32(7) = %d, want 7", got)
	}
	if got := clampNonNegative64(-1); got != 0 {
		t.Errorf("clampNonNegative64(-1) = %d, want 0", got)
	}
}

// TestNewLinux_RejectsMissingVMs ensures the constructor surfaces
// a usable error rather than panicking when wiring is incomplete.
func TestNewLinux_RejectsMissingVMs(t *testing.T) {
	_, err := NewLinux(CollectorDeps{Migration: config.MigrationConfig{}})
	if err == nil {
		t.Fatal("expected error when VMs lister is nil")
	}
}

type stubPublishedListenerReporter struct{ reports []PublishedListenerReport }

func (s stubPublishedListenerReporter) PublishedListenerReports() []PublishedListenerReport {
	return s.reports
}

// TestCollect_IncludesPublishedListeners confirms the collector folds the
// published-listener bind verdicts (from the PublishedListenerReporter seam)
// into Report.PublishedListeners for the heartbeat up-channel — one bound and
// one failed listener both survive.
func TestCollect_IncludesPublishedListeners(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cpuinfo"), []byte(syntheticCPUInfo), 0o644); err != nil {
		t.Fatalf("write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meminfo"), []byte(syntheticMemInfo), 0o644); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	lbBound, lbFailed := uuid.New(), uuid.New()
	want := []PublishedListenerReport{
		{LBID: lbBound, Port: 8080, Bound: true},
		{LBID: lbFailed, Port: 8081, Bound: false, Error: "bind: address already in use"},
	}
	c := &LinuxCollector{
		procPath:           dir,
		vms:                stubLister{},
		agentVersion:       "test",
		architecture:       "amd64",
		publishedListeners: stubPublishedListenerReporter{reports: want},
	}

	rep, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if diff := cmp.Diff(want, rep.PublishedListeners); diff != "" {
		t.Errorf("Report.PublishedListeners mismatch (-want +got):\n%s", diff)
	}
}

const syntheticCPUInfo = `processor	: 0
vendor_id	: AuthenticAMD
model name	: AMD EPYC 9554
flags		: fpu vme de pse tsc msr

processor	: 1
vendor_id	: AuthenticAMD
model name	: AMD EPYC 9554
flags		: fpu vme de pse tsc msr

processor	: 2
vendor_id	: AuthenticAMD
model name	: AMD EPYC 9554
flags		: fpu vme de pse tsc msr

processor	: 3
vendor_id	: AuthenticAMD
model name	: AMD EPYC 9554
flags		: fpu vme de pse tsc msr
`

const syntheticMemInfo = `MemTotal:       16777216 kB
MemFree:         1234567 kB
MemAvailable:    9876543 kB
`

// TestCollectCompressedSwapFromObserver confirms the collector maps the injected
// zram observation into caps.CompressedSwap. The seam is injected so the test
// never reads the host's real swap state.
func TestCollectCompressedSwapFromObserver(t *testing.T) {
	c := &LinuxCollector{
		procPath:     t.TempDir(),
		vms:          stubLister{},
		agentVersion: "test",
		architecture: "amd64",
		zramObserve: func() *zram.Active {
			return &zram.Active{Kind: "zram", SizeMib: 768, MemLimitMib: 256, Algorithm: "zstd"}
		},
	}

	rep, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	got := rep.Capabilities.CompressedSwap
	if got == nil || got.MemLimitMib != 256 || got.Algorithm != "zstd" {
		t.Fatalf("CompressedSwap = %+v, want mem_limit 256 / zstd", got)
	}
}

// TestCollectCompressedSwapNilWhenObserverNil confirms a nil observation (the
// safety net is off) leaves caps.CompressedSwap nil, so it serializes absent.
func TestCollectCompressedSwapNilWhenObserverNil(t *testing.T) {
	c := &LinuxCollector{
		procPath:     t.TempDir(),
		vms:          stubLister{},
		agentVersion: "test",
		architecture: "amd64",
		zramObserve:  func() *zram.Active { return nil },
	}

	rep, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if rep.Capabilities.CompressedSwap != nil {
		t.Errorf("CompressedSwap = %+v, want nil", rep.Capabilities.CompressedSwap)
	}
}
