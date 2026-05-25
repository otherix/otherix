// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
)

// Firmware describes a firmware blob present on the mock node. Mirrors
// agentapi.NodeInfoFirmware so the test API does not re-export the
// generated types — a future codegen change will not fan out into
// callers.
type Firmware struct {
	Name             string
	Architecture     string // "amd64" | "arm64"
	Type             string // "bios" | "uefi"
	CodePath         string
	VarsTemplatePath string
	SecureBoot       bool
}

// StoragePool is the test API view of a storage pool registered with
// the mock. ReportedAt is set internally by the mock on AddStoragePool
// and refreshed by SetPoolCapacity; callers do not populate it.
type StoragePool struct {
	ID             uuid.UUID
	Name           string
	Type           string // "local_dir" — currently the only enum value supported.
	Path           string
	CapacityBytes  int64
	AvailableBytes int64
}

// CachedImage describes an image staged in a pool. LastUsedAt is
// optional in the wire schema; a zero value is omitted from the
// response.
type CachedImage struct {
	ChecksumSHA256 string
	Format         string // "qcow2" | "raw"
	SizeBytes      int64
	Path           string
	LastUsedAt     time.Time
	SourceURL      string
}

// PoolScanResult is the queued synthetic outcome the mock applies to
// the next StoragePoolsScan invocation against the matching pool.
// Tests seed a per-pool FIFO queue via AddPoolScanResult; an empty
// queue yields a default success with zero capacity / available
// bytes and a 100ms delay.
//
// Status accepts "success" or "failed"; an empty string defaults to
// "success". Delay is the total time-to-terminal — pollers see
// pending for the first half, running for the second half, and the
// terminal status thereafter (lazy projection at tasks.get time).
type PoolScanResult struct {
	Status         string
	CapacityBytes  int64
	AvailableBytes int64
	Error          *ErrorEnvelope
	Delay          time.Duration
}

// ImageImportResult is the queued synthetic outcome the mock applies
// to the next StorageImagesImport invocation against the matching
// (pool, checksum). Tests seed a per-(pool, checksum) FIFO queue via
// AddImageImportResult.
//
// Status accepts "success" or "failed"; an empty string defaults to
// "success". SizeBytes / Format default to a 1 MiB qcow2 file when
// zero; Delay defaults to defaultPoolScanDelay (100ms) when zero —
// the same lazy time-keyed projection model the scan task uses.
//
// Pre-existence rule: when the requested checksum is already staged
// in the pool (via AddImage or a previous successful import), the
// import handler emits a terminal-success projection without
// consuming the FIFO queue. Tests that need to drive the
// pre-existing-content path keep their queue clean; tests that need
// to drive a fresh-import outcome stage the result here.
type ImageImportResult struct {
	Status    string
	SizeBytes int64
	Format    string
	Error     *ErrorEnvelope
	Delay     time.Duration
	// ComputedChecksumSHA256 is the checksum the mock surfaces in
	// task.result.checksum_sha256 when the request lands in compute
	// mode (`expected_checksum_sha256` null/absent/empty). Verify-mode
	// requests echo the request's expected value regardless of what
	// this field carries. Test authors driving compute-mode scenarios
	// populate this so the resulting storage_images row carries the
	// right hash; an empty value in compute mode falls back to a
	// deterministic synthetic checksum (see computeFallbackChecksum)
	// so tests that don't care about the value still get a valid
	// lowercase-hex string.
	ComputedChecksumSHA256 string
}

// ErrorEnvelope is the inner-error envelope shape the agent Task
// surfaces under task.error on failed / cancelled terminals.
type ErrorEnvelope struct {
	Code    string
	Message string
	Details map[string]any
}

// AgentVM is the test API view of a VM the mock has materialised on
// this node. The wire shape on the GET / LIST surface mirrors the
// Iteration 1 agent's hand-written `vmView` (id, name, vcpus,
// memory_mb, pool_id, architecture, status, created_at, updated_at) —
// the richer agentapi.VMSpec shape is a future design target and is
// deliberately not surfaced through the MVP wire.
//
// Tests inspect the inventory through StoredVM / ListStoredVMs;
// production code uses the HTTP surface.
type AgentVM struct {
	ID           uuid.UUID
	Name         string
	VCPUs        int
	MemoryMB     int
	PoolName     string
	Architecture string
	Status       string
	// UserData (L3) records the cloud-init blob the CP-side resolver
	// shipped with the create request. Empty when the operator left
	// both vm.user_data and template.cloud_init_user_data unset.
	// Surfaced through StoredVM so integration tests can
	// assert L3 resolution end-to-end.
	UserData  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// VMCreateResult is the queued synthetic outcome the mock applies to
// the next VmsCreate invocation against the matching VM uuid (the
// CP-minted id supplied in the request body). Tests seed a per-uuid
// FIFO queue via AddVMCreateResult; an empty queue yields a default
// success that materialises an AgentVM derived from the request body
// (architecture defaults to the mock node's architecture, status to
// "running"). Status accepts "success" or "failed"; an empty string
// defaults to "success". Delay defaults to defaultPoolScanDelay
// (100ms) when zero — the same lazy time-keyed projection model the
// scan task uses.
//
// Architecture and Status, when non-empty, override the defaults
// applied to the materialised AgentVM.
type VMCreateResult struct {
	Status       string
	Architecture string
	VMStatus     string
	Error        *ErrorEnvelope
	Delay        time.Duration
}

// VMDeleteResult is the queued synthetic outcome the mock applies to
// the next VmsDelete invocation against the matching VM uuid. Tests
// seed a per-uuid FIFO queue via AddVMDeleteResult. Default success
// removes the AgentVM entry from state.storedVMs on terminal-success.
type VMDeleteResult struct {
	Status string
	Error  *ErrorEnvelope
	Delay  time.Duration
}

// VMLifecycleResult is the queued synthetic outcome the mock applies
// to the next async lifecycle invocation (vm.start / vm.stop /
// vm.poweroff / vm.reboot) against the matching VM name. Tests seed
// per-(name, op) FIFO queues via AddVMLifecycleResult. Default success
// transitions the AgentVM entry's Status field per op semantics; tests
// can override `NewStatus` (most useful for verifying that stop
// timeout failure leaves the inventory entry in `running` rather than
// `stopped`). On Status="failed" the materialise hook is skipped and
// the task surfaces the Error envelope verbatim.
type VMLifecycleResult struct {
	Status    string
	NewStatus string // overrides the per-op default transition; "" → default
	Error     *ErrorEnvelope
	Delay     time.Duration
}

// MigrationCapability describes the inter-agent migration ingress
// advertised by the mock. Default is host=127.0.0.1 with the
// canonical 49152-49251 range.
type MigrationCapability struct {
	Host           string
	PortRangeStart int
	PortRangeEnd   int
	PortsInUse     int
	PortsAvailable int
}

// firmwareToAPI converts a local Firmware into the codegen wire type.
func firmwareToAPI(f Firmware) agentapi.NodeInfoFirmware {
	out := agentapi.NodeInfoFirmware{
		Name:         f.Name,
		Architecture: agentapi.NodeInfoFirmwareArchitecture(f.Architecture),
		Type:         agentapi.NodeInfoFirmwareType(f.Type),
		CodePath:     f.CodePath,
	}
	if f.VarsTemplatePath != "" {
		v := f.VarsTemplatePath
		out.VarsTemplatePath = &v
	}
	if f.SecureBoot {
		b := true
		out.SecureBoot = &b
	}
	return out
}

// poolToAPI converts an internal storagePool into a StoragePoolReport.
// CapacityBytes / AvailableBytes are zero-as-omitted: a zero value
// surfaces as a nil pointer (capacity unknown), not as a literal zero
// in the response — matching how a real agent reports an unmeasured
// pool.
func poolToAPI(p storagePool) agentapi.StoragePoolReport {
	out := agentapi.StoragePoolReport{
		ID:         p.ID,
		Name:       p.Name,
		Type:       agentapi.StoragePoolReportType(p.Type),
		Path:       p.Path,
		ReportedAt: p.reportedAt,
	}
	if p.CapacityBytes > 0 {
		c := p.CapacityBytes
		out.CapacityBytes = &c
	}
	if p.AvailableBytes > 0 {
		a := p.AvailableBytes
		out.AvailableBytes = &a
	}
	return out
}

// imageToAPI converts a CachedImage into the codegen wire type.
func imageToAPI(c CachedImage) agentapi.CachedImage {
	out := agentapi.CachedImage{
		ChecksumSha256: c.ChecksumSHA256,
		Format:         agentapi.CachedImageFormat(c.Format),
		SizeBytes:      c.SizeBytes,
		Path:           c.Path,
	}
	if !c.LastUsedAt.IsZero() {
		t := c.LastUsedAt
		out.LastUsedAt = &t
	}
	if c.SourceURL != "" {
		u := c.SourceURL
		out.SourceURL = &u
	}
	return out
}

// migrationToAPI converts a MigrationCapability into the codegen type.
func migrationToAPI(m MigrationCapability) agentapi.MigrationCapability {
	out := agentapi.MigrationCapability{
		Host:           m.Host,
		PortRangeStart: m.PortRangeStart,
		PortRangeEnd:   m.PortRangeEnd,
	}
	if m.PortsInUse > 0 {
		v := m.PortsInUse
		out.PortsInUse = &v
	}
	if m.PortsAvailable > 0 {
		v := m.PortsAvailable
		out.PortsAvailable = &v
	}
	return out
}
