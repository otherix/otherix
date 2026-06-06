// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// TestValidateCreateRequest covers the field-level invariants the
// API edge enforces before any DB work. Each row exercises one
// rejection branch; happy-path vetting lives in the firmware /
// schedule tests that need a store fake.
func TestValidateCreateRequest(t *testing.T) {
	t.Parallel()

	validSHA := strings.Repeat("ab", 32) // 64 chars lowercase hex
	base := func() vmCreateRequest {
		return vmCreateRequest{
			Name:         "demo",
			ImageURL:     "https://example.test/img.qcow2",
			Architecture: "amd64",
			Pool:         "default",
			VCPUs:        2,
			MemoryMB:     2048,
		}
	}

	cases := []struct {
		name string
		req  vmCreateRequest
		ok   bool
	}{
		{name: "happy path", req: base(), ok: true},
		{
			name: "happy path with valid sha + format + disk",
			req: func() vmCreateRequest {
				r := base()
				r.ImageSHA256 = validSHA
				r.Format = "qcow2"
				r.DiskGiB = 20
				return r
			}(),
			ok: true,
		},
		{name: "empty name", req: func() vmCreateRequest { r := base(); r.Name = ""; return r }()},
		{
			name: "name too long",
			req:  func() vmCreateRequest { r := base(); r.Name = string(make([]byte, 256)); return r }(),
		},
		{name: "empty image_url", req: func() vmCreateRequest { r := base(); r.ImageURL = ""; return r }()},
		{name: "missing arch", req: func() vmCreateRequest { r := base(); r.Architecture = ""; return r }()},
		{name: "bad arch", req: func() vmCreateRequest { r := base(); r.Architecture = "riscv"; return r }()},
		{name: "arm64 ok", req: func() vmCreateRequest { r := base(); r.Architecture = "arm64"; return r }(), ok: true},
		{name: "bad sha (short)", req: func() vmCreateRequest { r := base(); r.ImageSHA256 = "abcd"; return r }()},
		{name: "bad sha (uppercase)", req: func() vmCreateRequest { r := base(); r.ImageSHA256 = strings.ToUpper(validSHA); return r }()},
		{name: "bad format", req: func() vmCreateRequest { r := base(); r.Format = "vmdk"; return r }()},
		{name: "negative disk", req: func() vmCreateRequest { r := base(); r.DiskGiB = -1; return r }()},
		{
			// Empty pool is admitted - the handler substitutes the cluster
			// default at runtime, or returns 400 default_pool_not_set.
			name: "empty pool ok (cluster default fallback)",
			req:  func() vmCreateRequest { r := base(); r.Pool = ""; return r }(),
			ok:   true,
		},
		{name: "vcpus too small", req: func() vmCreateRequest { r := base(); r.VCPUs = 0; return r }()},
		{name: "vcpus too large", req: func() vmCreateRequest { r := base(); r.VCPUs = 129; return r }()},
		{name: "memory too small", req: func() vmCreateRequest { r := base(); r.MemoryMB = 64; return r }()},
		{name: "memory too large", req: func() vmCreateRequest { r := base(); r.MemoryMB = 1 << 30; return r }()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &captureWriter{}
			got := validateCreateRequest(rec, fakeRequest(), tc.req)
			if got != tc.ok {
				t.Errorf("validateCreateRequest(%s) = %v, want %v", tc.name, got, tc.ok)
			}
		})
	}
}

// firmwareStoreStub satisfies the handler's Store interface for the
// resolveFirmware unit tests. It embeds Store (nil) so only the two firmware
// methods exercised here have bodies; any other call panics.
type firmwareStoreStub struct {
	Store
	byID      map[uuid.UUID]store.Firmware
	byArch    map[store.FirmwareType]store.Firmware
	byIDErr   error
	byArchErr error
}

func (s *firmwareStoreStub) FirmwareByID(_ context.Context, id uuid.UUID) (store.Firmware, error) {
	if s.byIDErr != nil {
		return store.Firmware{}, s.byIDErr
	}
	fw, ok := s.byID[id]
	if !ok {
		return store.Firmware{}, store.ErrNotFound
	}
	return fw, nil
}

func (s *firmwareStoreStub) DefaultFirmwareForArchType(_ context.Context, _ store.CPUArch, ftype store.FirmwareType) (store.Firmware, error) {
	if s.byArchErr != nil {
		return store.Firmware{}, s.byArchErr
	}
	fw, ok := s.byArch[ftype]
	if !ok {
		return store.Firmware{}, store.ErrNotFound
	}
	return fw, nil
}

// TestResolveFirmware covers the firmware-resolution truth table: an explicit
// firmware_id wins; otherwise the default for (arch, type) where type defaults
// to uefi unless firmware=="bios"; bad inputs surface typed sentinels.
func TestResolveFirmware(t *testing.T) {
	t.Parallel()

	explicitID := uuid.New()
	uefiID := uuid.New()
	biosID := uuid.New()
	st := &firmwareStoreStub{
		byID: map[uuid.UUID]store.Firmware{explicitID: {ID: explicitID}},
		byArch: map[store.FirmwareType]store.Firmware{
			store.FirmwareTypeUefi: {ID: uefiID},
			store.FirmwareTypeBios: {ID: biosID},
		},
	}
	h := &Handler{store: st, log: discardLog()}

	cases := []struct {
		name       string
		firmware   string
		firmwareID string
		wantID     uuid.UUID
		wantErr    error
	}{
		{name: "explicit id wins", firmwareID: explicitID.String(), wantID: explicitID},
		{name: "default uefi when firmware empty", wantID: uefiID},
		{name: "explicit uefi", firmware: "uefi", wantID: uefiID},
		{name: "explicit bios", firmware: "bios", wantID: biosID},
		{name: "bad uuid", firmwareID: "not-a-uuid", wantErr: errFirmwareBadID},
		{name: "bad firmware type", firmware: "coreboot", wantErr: errFirmwareBadType},
		{name: "explicit id missing", firmwareID: uuid.New().String(), wantErr: errFirmwareNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := h.resolveFirmware(context.Background(), store.CpuArchAmd64, tc.firmware, tc.firmwareID)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("resolveFirmware err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveFirmware err = %v, want nil", err)
			}
			if got == nil || *got != tc.wantID {
				t.Errorf("resolveFirmware id = %v, want %s", got, tc.wantID)
			}
		})
	}
}

// TestResolveFirmwareNoDefault pins the no-default-for-arch/type path: firmware
// is optional, so with no seeded default the resolver returns (nil, nil) and the
// VM is created without a firmware row (boots with the agent/qemu default).
func TestResolveFirmwareNoDefault(t *testing.T) {
	t.Parallel()
	st := &firmwareStoreStub{byArch: map[store.FirmwareType]store.Firmware{}}
	h := &Handler{store: st, log: discardLog()}
	got, err := h.resolveFirmware(context.Background(), store.CpuArchArm64, "", "")
	if err != nil {
		t.Errorf("resolveFirmware err = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("resolveFirmware id = %v, want nil (no firmware)", got)
	}
}

// placementReaderFake returns one eligible local_dir pool on one ready node so a
// scheduleAndEnqueueCreate plan callback can run the real scheduler against a
// trivial candidate set. Only the methods SchedulePlacement touches in the
// happy path are populated; the pressure lists return empty.
type placementReaderFake struct {
	pool store.ListEligiblePoolsByNameRow
}

func (f *placementReaderFake) AcquirePlacementLock(context.Context, int64) error { return nil }

func (f *placementReaderFake) ListEligiblePoolsByName(context.Context, string) ([]store.ListEligiblePoolsByNameRow, error) {
	return []store.ListEligiblePoolsByNameRow{f.pool}, nil
}

func (f *placementReaderFake) ListMemoryPressuredCandidatesByName(context.Context, string) ([]store.ListMemoryPressuredCandidatesByNameRow, error) {
	return nil, nil
}

func (f *placementReaderFake) ListSystemDiskPressuredCandidatesByName(context.Context, string) ([]store.ListSystemDiskPressuredCandidatesByNameRow, error) {
	return nil, nil
}

func (f *placementReaderFake) ListDiskPressuredPoolsByName(context.Context, string) ([]store.ListDiskPressuredPoolsByNameRow, error) {
	return nil, nil
}

func (f *placementReaderFake) ListStoragePoolsByName(context.Context, string) ([]store.StoragePool, error) {
	return nil, nil
}

func (f *placementReaderFake) CountRunningVMsByNode(context.Context, *uuid.UUID) (int64, error) {
	return 0, nil
}

func (f *placementReaderFake) ListNetworkNodeStatusByNode(context.Context, uuid.UUID) ([]store.NetworkNodeStatus, error) {
	return nil, nil
}

// TestScheduleAndEnqueueCreateBuildsImageWrites pins the create-enqueue seam:
// the plan callback builds the VM, disk and job-args rows from the image fields
// on the request plus the resolved firmware id. It runs the real scheduler over
// a one-pool/one-node fake and inspects the captured VMCreateWrites.
func TestScheduleAndEnqueueCreateBuildsImageWrites(t *testing.T) {
	t.Parallel()

	nodeID := uuid.New()
	poolID := uuid.New()
	cpuTotal := int32(64)
	memTotal := int64(262144)
	reader := &placementReaderFake{pool: store.ListEligiblePoolsByNameRow{
		PoolEffectiveCapacity: store.PoolEffectiveCapacity{ID: poolID, NodeID: nodeID, Name: "default", Type: "local_dir"},
		NodeEffectiveAvailability: store.NodeEffectiveAvailability{
			ID: nodeID, Name: "node-x", Architecture: store.CpuArchAmd64,
			AdvertisedEndpoint: "https://node-x:8443", Status: store.NodeStatusReady,
			CPUCoresTotal: &cpuTotal, CPUCoresAvailable: &cpuTotal,
			MemoryTotalMib: &memTotal, MemoryAvailableMib: &memTotal,
		},
	}}

	var captured store.VMCreateWrites
	st := &createScheduledVMStub{
		createScheduledVM: func(_ context.Context, plan func(store.PlacementReader) (store.VMCreateWrites, error)) (uuid.UUID, error) {
			w, err := plan(reader)
			if err != nil {
				return uuid.Nil, err
			}
			captured = w
			return w.VM.ID, nil
		},
	}
	h := &Handler{store: st, log: discardLog()}

	firmwareID := uuid.New()
	sha := "ab" + strings.Repeat("cd", 31) // 64 chars lowercase hex
	_, err := h.scheduleAndEnqueueCreate(context.Background(), scheduleInputs{
		Caller:     &auth.User{ID: uuid.New(), Role: auth.RoleDeveloper},
		FirmwareID: &firmwareID,
		PoolName:   "default",
		Req: vmCreateRequest{
			Name:         "demo",
			ImageURL:     "https://example.test/img.qcow2",
			ImageSHA256:  sha,
			Architecture: "amd64",
			Format:       "qcow2",
			DiskGiB:      20,
			VCPUs:        2,
			MemoryMB:     2048,
		},
	})
	if err != nil {
		t.Fatalf("scheduleAndEnqueueCreate: %v", err)
	}

	if captured.VM.ImageURL != "https://example.test/img.qcow2" {
		t.Errorf("VM.ImageURL = %q, want image url", captured.VM.ImageURL)
	}
	if captured.VM.ImageFormat != store.ImageFormatQcow2 {
		t.Errorf("VM.ImageFormat = %q, want qcow2", captured.VM.ImageFormat)
	}
	if captured.VM.Architecture != store.CpuArchAmd64 {
		t.Errorf("VM.Architecture = %q, want amd64", captured.VM.Architecture)
	}
	if captured.VM.FirmwareID == nil || *captured.VM.FirmwareID != firmwareID {
		t.Errorf("VM.FirmwareID = %v, want %s", captured.VM.FirmwareID, firmwareID)
	}
	wantSHA, _ := hex.DecodeString(sha)
	if !bytes.Equal(captured.VM.ImageSHA256, wantSHA) {
		t.Errorf("VM.ImageSHA256 = %x, want %x", captured.VM.ImageSHA256, wantSHA)
	}
	if captured.Disk.SourceKind != "image" {
		t.Errorf("Disk.SourceKind = %q, want image", captured.Disk.SourceKind)
	}
	if captured.Disk.SizeGib != 20 {
		t.Errorf("Disk.SizeGib = %d, want 20", captured.Disk.SizeGib)
	}
	// The image source lives on the VM/disk rows (asserted above); the job
	// payload carries only the row identifiers.
	if captured.VM.ImageURL != "https://example.test/img.qcow2" {
		t.Errorf("VM.ImageURL = %q, want https://example.test/img.qcow2", captured.VM.ImageURL)
	}
	if captured.VM.ImageFormat != "qcow2" {
		t.Errorf("VM.ImageFormat = %q, want qcow2", captured.VM.ImageFormat)
	}
	job, ok := captured.Job.(VMCreateArgs)
	if !ok {
		t.Fatalf("Job is %T, want VMCreateArgs", captured.Job)
	}
	if job.NodeID != nodeID || job.PoolID != poolID {
		t.Errorf("VMCreateArgs placement = node %s pool %s, want %s / %s", job.NodeID, job.PoolID, nodeID, poolID)
	}
}

// TestBuildNoEligibleDetails verifies the 409 envelope's
// `filtered_due_to_pressure` payload: it renders each nullable pressure
// timestamp only when its condition is active, and surfaces the `pool`
// name on pool-scoped entries. The nil-safety guard on the three
// `*time.Time` fields prevents `.IsZero()` from being called on a nil
// pointer when only system_disk_pressure or disk_pressure is active.
func TestBuildNoEligibleDetails(t *testing.T) {
	t.Parallel()

	mem := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	sys := time.Date(2026, 5, 12, 12, 5, 0, 0, time.UTC)
	pool := time.Date(2026, 5, 12, 12, 10, 0, 0, time.UTC)

	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

	cases := []struct {
		name string
		err  error
		want map[string]any
	}{
		{
			name: "nil error returns nil",
			err:  nil,
			want: nil,
		},
		{
			name: "bare ErrNoEligibleNodes returns nil",
			err:  scheduler.ErrNoEligibleNodes,
			want: nil,
		},
		{
			name: "wrapped ErrNoEligibleNodes without pressure detail returns nil",
			err:  fmt.Errorf("upstream: %w", scheduler.ErrNoEligibleNodes),
			want: nil,
		},
		{
			name: "empty pressure detail returns nil",
			err:  scheduler.NewNodePressureErrorForTest(scheduler.NodePressureDetail{Nodes: nil}),
			want: nil,
		},
		{
			name: "memory pressure only",
			err: scheduler.NewNodePressureErrorForTest(scheduler.NodePressureDetail{
				Nodes: []scheduler.PressuredNode{{
					Node:                "node-a",
					Conditions:          []string{"memory_pressure"},
					MemoryPressureSince: &mem,
				}},
			}),
			want: map[string]any{
				"reason": "node_pressure",
				"filtered_due_to_pressure": []map[string]any{{
					"node":                  "node-a",
					"conditions":            []string{"memory_pressure"},
					"memory_pressure_since": rfc(mem),
				}},
			},
		},
		{
			name: "system_disk pressure only (A3 regression)",
			err: scheduler.NewNodePressureErrorForTest(scheduler.NodePressureDetail{
				Nodes: []scheduler.PressuredNode{{
					Node:                    "node-a",
					Conditions:              []string{"system_disk_pressure"},
					SystemDiskPressureSince: &sys,
				}},
			}),
			want: map[string]any{
				"reason": "node_pressure",
				"filtered_due_to_pressure": []map[string]any{{
					"node":                       "node-a",
					"conditions":                 []string{"system_disk_pressure"},
					"system_disk_pressure_since": rfc(sys),
				}},
			},
		},
		{
			name: "pool disk pressure only (A3 regression)",
			err: scheduler.NewNodePressureErrorForTest(scheduler.NodePressureDetail{
				Nodes: []scheduler.PressuredNode{{
					Node:              "node-a",
					Pool:              "pool-dev",
					Conditions:        []string{"disk_pressure"},
					DiskPressureSince: &pool,
				}},
			}),
			want: map[string]any{
				"reason": "node_pressure",
				"filtered_due_to_pressure": []map[string]any{{
					"node":                "node-a",
					"pool":                "pool-dev",
					"conditions":          []string{"disk_pressure"},
					"disk_pressure_since": rfc(pool),
				}},
			},
		},
		{
			name: "combined memory + system_disk on same node",
			err: scheduler.NewNodePressureErrorForTest(scheduler.NodePressureDetail{
				Nodes: []scheduler.PressuredNode{{
					Node:                    "node-a",
					Conditions:              []string{"memory_pressure", "system_disk_pressure"},
					MemoryPressureSince:     &mem,
					SystemDiskPressureSince: &sys,
				}},
			}),
			want: map[string]any{
				"reason": "node_pressure",
				"filtered_due_to_pressure": []map[string]any{{
					"node":                       "node-a",
					"conditions":                 []string{"memory_pressure", "system_disk_pressure"},
					"memory_pressure_since":      rfc(mem),
					"system_disk_pressure_since": rfc(sys),
				}},
			},
		},
		{
			name: "combined non-memory: node system_disk + pool disk (A3 regression)",
			err: scheduler.NewNodePressureErrorForTest(scheduler.NodePressureDetail{
				Nodes: []scheduler.PressuredNode{
					{
						Node:                    "node-a",
						Conditions:              []string{"system_disk_pressure"},
						SystemDiskPressureSince: &sys,
					},
					{
						Node:              "node-a",
						Pool:              "pool-dev",
						Conditions:        []string{"disk_pressure"},
						DiskPressureSince: &pool,
					},
				},
			}),
			want: map[string]any{
				"reason": "node_pressure",
				"filtered_due_to_pressure": []map[string]any{
					{
						"node":                       "node-a",
						"conditions":                 []string{"system_disk_pressure"},
						"system_disk_pressure_since": rfc(sys),
					},
					{
						"node":                "node-a",
						"pool":                "pool-dev",
						"conditions":          []string{"disk_pressure"},
						"disk_pressure_since": rfc(pool),
					},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := buildNoEligibleDetails(tc.err)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("buildNoEligibleDetails mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestBuildNoEligibleDetails_DoesNotPanicOnNilTimestamps is the
// targeted A3 regression net. The pre-fix implementation called
// `.IsZero()` on a nil *time.Time pointer; any pressure type other
// than memory (where MemoryPressureSince was always populated)
// triggered the nil-deref. Asserting "does not panic" in isolation
// catches future drift if a new pressure timestamp is added without
// matching nil-check.
func TestBuildNoEligibleDetails_DoesNotPanicOnNilTimestamps(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("buildNoEligibleDetails panicked: %v", r)
		}
	}()

	err := scheduler.NewNodePressureErrorForTest(scheduler.NodePressureDetail{
		Nodes: []scheduler.PressuredNode{{
			Node:       "node-a",
			Conditions: []string{"system_disk_pressure"},
			// All three *time.Time fields intentionally nil — exercises
			// the pre-fix panic site.
		}},
	})

	got := buildNoEligibleDetails(err)
	if got == nil {
		t.Fatalf("buildNoEligibleDetails returned nil; want envelope")
	}
	filtered, ok := got["filtered_due_to_pressure"].([]map[string]any)
	if !ok || len(filtered) != 1 {
		t.Fatalf("filtered_due_to_pressure shape mismatch: %v", got["filtered_due_to_pressure"])
	}
	// None of the *_pressure_since keys should be present when the
	// timestamp pointer was nil.
	for _, key := range []string{"memory_pressure_since", "system_disk_pressure_since", "disk_pressure_since"} {
		if _, ok := filtered[0][key]; ok {
			t.Errorf("filtered_due_to_pressure[0] unexpectedly contains %q with nil source", key)
		}
	}
	// And ensure the wrapped ErrNoEligibleNodes sentinel is still
	// detectable through the test helper's error chain (defensive — if
	// the helper's wrap pattern changes the extractor breaks silently).
	if !errors.Is(err, scheduler.ErrNoEligibleNodes) {
		t.Errorf("test helper error chain does not unwrap to ErrNoEligibleNodes")
	}
}

// TestGenerateLocalMAC asserts the minted MAC carries the QEMU 52:54:00 OUI,
// is locally administered + unicast, and varies across calls.
func TestGenerateLocalMAC(t *testing.T) {
	mac, err := generateLocalMAC()
	if err != nil {
		t.Fatalf("generateLocalMAC: %v", err)
	}
	if len(mac) != 6 {
		t.Fatalf("len(mac) = %d, want 6", len(mac))
	}
	if mac[0] != 0x52 || mac[1] != 0x54 || mac[2] != 0x00 {
		t.Errorf("mac OUI = %02x:%02x:%02x, want 52:54:00", mac[0], mac[1], mac[2])
	}
	// Locally administered (bit 1 set) + unicast (bit 0 clear) in the first octet.
	if mac[0]&0x02 == 0 {
		t.Errorf("mac first octet %02x is not locally administered", mac[0])
	}
	if mac[0]&0x01 != 0 {
		t.Errorf("mac first octet %02x is not unicast", mac[0])
	}
	other, err := generateLocalMAC()
	if err != nil {
		t.Fatalf("generateLocalMAC (2nd): %v", err)
	}
	if mac.String() == other.String() {
		t.Errorf("two generated MACs collided: %s", mac)
	}
}

// createScheduledVMStub satisfies the handler's Store interface for the
// createWithMACRetry unit tests. It embeds Store (nil) so only the one
// method exercised here needs a body; calling any other method panics,
// which is the intended guard for an unexpected dependency.
type createScheduledVMStub struct {
	Store
	createScheduledVM func(ctx context.Context, plan func(store.PlacementReader) (store.VMCreateWrites, error)) (uuid.UUID, error)
}

func (s *createScheduledVMStub) CreateScheduledVM(ctx context.Context, plan func(store.PlacementReader) (store.VMCreateWrites, error)) (uuid.UUID, error) {
	return s.createScheduledVM(ctx, plan)
}

// TestCreateWithMACRetry asserts a transient ErrVMNicMACConflict is
// re-minted: the helper re-invokes CreateScheduledVM (which re-rolls the
// MAC) until the store stops reporting a collision.
func TestCreateWithMACRetry(t *testing.T) {
	t.Parallel()

	calls := 0
	fake := &createScheduledVMStub{
		createScheduledVM: func(_ context.Context, _ func(store.PlacementReader) (store.VMCreateWrites, error)) (uuid.UUID, error) {
			calls++
			if calls < 3 {
				return uuid.Nil, store.ErrVMNicMACConflict
			}
			return uuid.New(), nil
		},
	}

	id, err := createWithMACRetry(context.Background(), fake, func(store.PlacementReader) (store.VMCreateWrites, error) {
		return store.VMCreateWrites{}, nil
	})
	if err != nil {
		t.Fatalf("createWithMACRetry: %v", err)
	}
	if id == uuid.Nil {
		t.Errorf("got nil id, want a created id")
	}
	if calls != 3 {
		t.Errorf("CreateScheduledVM calls = %d, want 3 (2 conflicts then success)", calls)
	}
}

// TestCreateWithMACRetryGivesUp asserts a persistent collision is bounded:
// after maxMACRetries attempts the helper returns the last
// ErrVMNicMACConflict rather than looping forever.
func TestCreateWithMACRetryGivesUp(t *testing.T) {
	t.Parallel()

	calls := 0
	fake := &createScheduledVMStub{
		createScheduledVM: func(_ context.Context, _ func(store.PlacementReader) (store.VMCreateWrites, error)) (uuid.UUID, error) {
			calls++
			return uuid.Nil, store.ErrVMNicMACConflict
		},
	}

	_, err := createWithMACRetry(context.Background(), fake, func(store.PlacementReader) (store.VMCreateWrites, error) {
		return store.VMCreateWrites{}, nil
	})
	if !errors.Is(err, store.ErrVMNicMACConflict) {
		t.Errorf("err = %v, want ErrVMNicMACConflict after exhausting retries", err)
	}
	if calls != 8 {
		t.Errorf("calls = %d, want 8 (maxMACRetries)", calls)
	}
}
