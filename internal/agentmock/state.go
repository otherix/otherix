// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors returned by the test API. The set is small by
// design — only these two outcomes are needed.
var (
	// ErrNotFound is returned by mutators that name a resource the
	// mock has not registered (e.g. AddImage on an unknown pool).
	ErrNotFound = errors.New("agentmock: not found")

	// ErrInvalidPath is returned by SetCapability when the path
	// language does not match a known capability field, or when the
	// supplied value has the wrong Go type for the chosen path.
	ErrInvalidPath = errors.New("agentmock: invalid capability path")
)

// state is the in-memory state of a Mock instance. All access is
// guarded by mu; helpers ending in Locked assume the lock is held by
// the caller. The split exists so public *Mock methods can lock once
// and delegate to a single bookkeeping helper without re-entry.
type state struct {
	mu sync.Mutex

	// Identity (immutable post-Start).
	nodeID       uuid.UUID
	architecture string
	agentVersion string
	startedAt    time.Time

	// NodeInfo capability fields (mutable through SetCapability).
	hostname           string
	cpuModel           string
	cpuFeatures        []string
	cpuCoresTotal      int
	cpuCoresAvailable  int
	memoryTotalMib     int64
	memoryAvailableMib int64
	kernelVersion      string
	qemuVersion        string
	kvmAvailable       bool
	nestedVirt         bool
	qemuBinaries       map[string]string
	hugepages2MiBTotal *int
	hugepages1GiBTotal *int
	labels             map[string]string

	// Per-resource collections.
	firmwares []Firmware
	migration MigrationCapability
	pools     map[string]storagePool
	images    map[string]map[string]CachedImage // pool name → checksum → image

	// Agent-task projection for storage_pool.scan. `tasks` is keyed
	// by the agent-side task uuid minted inside StoragePoolsScan;
	// `poolScanResults` is the per-pool FIFO queue test authors
	// populate via AddPoolScanResult.
	tasks           map[uuid.UUID]*agentTask
	poolScanResults map[string][]PoolScanResult

	// MVP Iteration 3 Phase A vm.create / vm.delete projection.
	// `storedVMs` is the on-node inventory observable through
	// VmsGet / VmsList; create-success materialises an entry, delete-
	// success removes it. `vmCreateResults` and `vmDeleteResults` are
	// per-name FIFO queues seeded by the test API — empty queue +
	// no pre-existing VM defaults to success. Per Pre-L1 Path D rekey
	// the agent's wire surface addresses VMs by name; the mock's
	// in-memory inventory mirrors that key so the materialisation
	// flow matches the agent contract end-to-end.
	storedVMs       map[string]AgentVM
	vmCreateResults map[string][]VMCreateResult
	vmDeleteResults map[string][]VMDeleteResult

	// L2 async lifecycle FIFO queues, keyed by (vmName, op) where op
	// is one of "start" / "stop" / "poweroff" / "reboot". Empty queue
	// yields a default success per vmLifecycleOpDefaultTransition. Per
	// L2 Step 5 the materialisation hook transitions storedVMs[name]
	// either to the per-op default phase or to VMLifecycleResult.NewStatus
	// if non-empty.
	vmLifecycleResults map[vmLifecycleKey][]VMLifecycleResult

	// Test fixture machinery (no wire surface).
	requests []RequestRecord
	inject   injectionState

	// Heartbeat protocol bookkeeping (drives optional `migration`
	// field per control-plane.yaml HeartbeatRequest semantics).
	heartbeatSuppressed    bool
	lastHeartbeatMigration *MigrationCapability
}

// storagePool extends the exported StoragePool with internal
// bookkeeping for the reported_at field.
type storagePool struct {
	StoragePool
	reportedAt time.Time
}

// vmLifecycleKey indexes the per-(vm name, op) FIFO queue of L2 async
// lifecycle outcomes. Op is one of "start" / "stop" / "poweroff" /
// "reboot"; per-op separation lets one VM stage a stop-timeout failure
// and a subsequent start success without interference.
type vmLifecycleKey struct {
	VMName string
	Op     string
}

// AddFirmware registers a firmware blob in the node's local
// catalogue. Reflected in the next /v1/info response and the next
// heartbeat.
func (m *Mock) AddFirmware(f Firmware) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	m.state.firmwares = append(m.state.firmwares, f)
}

// RemoveFirmware drops a firmware by (name, architecture, type).
// Returns false when no matching entry exists.
func (m *Mock) RemoveFirmware(name, architecture, fwType string) bool {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	for i, f := range m.state.firmwares {
		if f.Name == name && f.Architecture == architecture && f.Type == fwType {
			m.state.firmwares = append(m.state.firmwares[:i], m.state.firmwares[i+1:]...)
			return true
		}
	}
	return false
}

// AddStoragePool registers a pool. Keying is by pool **name**; the
// exported StoragePool.ID UUID is preserved as a traceability label
// surfaced in the agent's wire response. Replaces any existing pool
// with the same name.
func (m *Mock) AddStoragePool(p StoragePool) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	m.state.pools[p.Name] = storagePool{StoragePool: p, reportedAt: time.Now().UTC()}
	if _, ok := m.state.images[p.Name]; !ok {
		m.state.images[p.Name] = map[string]CachedImage{}
	}
}

// SetPoolCapacity updates a pool's capacity / available bytes and
// stamps reported_at. Returns ErrNotFound for an unregistered pool.
func (m *Mock) SetPoolCapacity(poolName string, capacityBytes, availableBytes int64) error {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	p, ok := m.state.pools[poolName]
	if !ok {
		return fmt.Errorf("%w: storage pool %s", ErrNotFound, poolName)
	}
	p.CapacityBytes = capacityBytes
	p.AvailableBytes = availableBytes
	p.reportedAt = time.Now().UTC()
	m.state.pools[poolName] = p
	return nil
}

// AddImage stages a cached image inside a pool. Returns ErrNotFound
// for an unregistered pool.
func (m *Mock) AddImage(poolName string, img CachedImage) error {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if _, ok := m.state.pools[poolName]; !ok {
		return fmt.Errorf("%w: storage pool %s", ErrNotFound, poolName)
	}
	bucket := m.state.images[poolName]
	bucket[img.ChecksumSHA256] = img
	return nil
}

// EvictImage drops a cached image by checksum from a pool. Returns
// false when no matching image is staged.
func (m *Mock) EvictImage(poolName, checksum string) bool {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	bucket, ok := m.state.images[poolName]
	if !ok {
		return false
	}
	if _, present := bucket[checksum]; !present {
		return false
	}
	delete(bucket, checksum)
	return true
}

// StoredImage returns the staged image for (poolName, checksum).
// The second return is false when the pool is unregistered or the
// image is not staged.
func (m *Mock) StoredImage(poolName, checksum string) (CachedImage, bool) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	bucket, ok := m.state.images[poolName]
	if !ok {
		return CachedImage{}, false
	}
	img, ok := bucket[checksum]
	return img, ok
}

// ListStoredImages returns a snapshot of the cached images staged in
// poolName. Order is not guaranteed (map iteration). Empty slice (not
// nil) for an unregistered or empty pool.
func (m *Mock) ListStoredImages(poolName string) []CachedImage {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	bucket := m.state.images[poolName]
	out := make([]CachedImage, 0, len(bucket))
	for _, img := range bucket {
		out = append(out, img)
	}
	return out
}

// AddVMCreateResult queues a synthetic outcome for the next VmsCreate
// call against vmName. Calls are FIFO per name; an empty queue and no
// pre-existing AgentVM yield a default success that materialises an
// AgentVM derived from the request body. See VMCreateResult docs for
// field semantics.
func (m *Mock) AddVMCreateResult(vmName string, r VMCreateResult) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if m.state.vmCreateResults == nil {
		m.state.vmCreateResults = map[string][]VMCreateResult{}
	}
	m.state.vmCreateResults[vmName] = append(m.state.vmCreateResults[vmName], r)
}

// AddVMDeleteResult queues a synthetic outcome for the next VmsDelete
// call against vmName. Calls are FIFO per name; an empty queue yields
// a default success that removes the AgentVM from state.storedVMs.
func (m *Mock) AddVMDeleteResult(vmName string, r VMDeleteResult) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if m.state.vmDeleteResults == nil {
		m.state.vmDeleteResults = map[string][]VMDeleteResult{}
	}
	m.state.vmDeleteResults[vmName] = append(m.state.vmDeleteResults[vmName], r)
}

// AddVMLifecycleResult queues a synthetic outcome for the next async
// lifecycle invocation against (vmName, op). Calls are FIFO per
// (name, op). Empty queue yields a default success that transitions
// the inventory entry per vmLifecycleOpDefaultTransition.
func (m *Mock) AddVMLifecycleResult(vmName, op string, r VMLifecycleResult) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if m.state.vmLifecycleResults == nil {
		m.state.vmLifecycleResults = map[vmLifecycleKey][]VMLifecycleResult{}
	}
	key := vmLifecycleKey{VMName: vmName, Op: op}
	m.state.vmLifecycleResults[key] = append(m.state.vmLifecycleResults[key], r)
}

// StoredVM returns the materialised AgentVM for vmName. The second
// return is false when the VM is not in the inventory. Test
// introspection — used to assert post-create / post-delete state in
// vm-lifecycle tests.
func (m *Mock) StoredVM(vmName string) (AgentVM, bool) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	v, ok := m.state.storedVMs[vmName]
	return v, ok
}

// SetStoredVM seeds the inventory with vm. The handlers that key on
// StoredVM (the console-stream / logs paths) see the entry the same
// way they would after a VmsCreate, without forcing tests to drive
// the full create flow when they only need a target VM to exist.
func (m *Mock) SetStoredVM(vm AgentVM) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if m.state.storedVMs == nil {
		m.state.storedVMs = map[string]AgentVM{}
	}
	m.state.storedVMs[vm.Name] = vm
}

// ListStoredVMs returns a snapshot of the AgentVMs currently in the
// inventory. Order is not guaranteed (map iteration); callers that
// need deterministic ordering sort by ID. Empty slice (not nil) for
// an empty inventory.
func (m *Mock) ListStoredVMs() []AgentVM {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	out := make([]AgentVM, 0, len(m.state.storedVMs))
	for _, v := range m.state.storedVMs {
		out = append(out, v)
	}
	return out
}

// SetMigrationCapability replaces the advertised migration capability.
// Reflected in /v1/info and (when changed) the next heartbeat.
func (m *Mock) SetMigrationCapability(c MigrationCapability) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	m.state.migration = c
}

// SetCapability overrides one capability field by JSON path. The path
// language is narrow — top-level fields plus `labels.<key>`. Anything
// else returns ErrInvalidPath; the same sentinel covers a value with
// the wrong Go type for the chosen path.
func (m *Mock) SetCapability(path string, value any) error {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()
	if key, ok := strings.CutPrefix(path, "labels."); ok {
		return m.setLabelLocked(key, path, value)
	}
	setter, ok := capabilitySetters()[path]
	if !ok {
		return fmt.Errorf("%w: %s", ErrInvalidPath, path)
	}
	return setter(m.state, path, value)
}

func (m *Mock) setLabelLocked(key, path string, value any) error {
	if key == "" {
		return fmt.Errorf("%w: empty labels key", ErrInvalidPath)
	}
	s, ok := value.(string)
	if !ok {
		return fmt.Errorf("%w: %s expects string, got %T", ErrInvalidPath, path, value)
	}
	if m.state.labels == nil {
		m.state.labels = map[string]string{}
	}
	m.state.labels[key] = s
	return nil
}

// capabilitySetters returns the dispatch table for SetCapability.
// Defining it as a function — not a package-level var — avoids
// referencing state fields outside any concrete instance.
func capabilitySetters() map[string]func(*state, string, any) error {
	return map[string]func(*state, string, any) error{
		"hostname":             func(s *state, p string, v any) error { return assignString(&s.hostname, p, v) },
		"cpu_model":            func(s *state, p string, v any) error { return assignString(&s.cpuModel, p, v) },
		"kernel_version":       func(s *state, p string, v any) error { return assignString(&s.kernelVersion, p, v) },
		"qemu_version":         func(s *state, p string, v any) error { return assignString(&s.qemuVersion, p, v) },
		"cpu_features":         setCPUFeatures,
		"cpu_cores_total":      func(s *state, p string, v any) error { return assignInt(&s.cpuCoresTotal, p, v) },
		"cpu_cores_available":  func(s *state, p string, v any) error { return assignInt(&s.cpuCoresAvailable, p, v) },
		"memory_total_mib":     func(s *state, p string, v any) error { return assignInt64(&s.memoryTotalMib, p, v) },
		"memory_available_mib": func(s *state, p string, v any) error { return assignInt64(&s.memoryAvailableMib, p, v) },
		"kvm_available":        func(s *state, p string, v any) error { return assignBool(&s.kvmAvailable, p, v) },
		"nested_virt":          func(s *state, p string, v any) error { return assignBool(&s.nestedVirt, p, v) },
		"qemu_binaries":        setQEMUBinaries,
		"hugepages_2mib_total": func(s *state, p string, v any) error { return assignNullableInt(&s.hugepages2MiBTotal, p, v) },
		"hugepages_1gib_total": func(s *state, p string, v any) error { return assignNullableInt(&s.hugepages1GiBTotal, p, v) },
	}
}

func setCPUFeatures(s *state, path string, value any) error {
	v, ok := value.([]string)
	if !ok {
		return fmt.Errorf("%w: %s expects []string, got %T", ErrInvalidPath, path, value)
	}
	s.cpuFeatures = append(s.cpuFeatures[:0], v...)
	return nil
}

func setQEMUBinaries(s *state, path string, value any) error {
	v, ok := value.(map[string]string)
	if !ok {
		return fmt.Errorf("%w: %s expects map[string]string, got %T", ErrInvalidPath, path, value)
	}
	s.qemuBinaries = cloneStringMap(v)
	return nil
}

func assignString(dst *string, path string, value any) error {
	v, ok := value.(string)
	if !ok {
		return fmt.Errorf("%w: %s expects string, got %T", ErrInvalidPath, path, value)
	}
	*dst = v
	return nil
}

func assignInt(dst *int, path string, value any) error {
	v, ok := value.(int)
	if !ok {
		return fmt.Errorf("%w: %s expects int, got %T", ErrInvalidPath, path, value)
	}
	*dst = v
	return nil
}

func assignInt64(dst *int64, path string, value any) error {
	switch v := value.(type) {
	case int64:
		*dst = v
		return nil
	case int:
		*dst = int64(v)
		return nil
	}
	return fmt.Errorf("%w: %s expects int64, got %T", ErrInvalidPath, path, value)
}

func assignBool(dst *bool, path string, value any) error {
	v, ok := value.(bool)
	if !ok {
		return fmt.Errorf("%w: %s expects bool, got %T", ErrInvalidPath, path, value)
	}
	*dst = v
	return nil
}

func assignNullableInt(dst **int, path string, value any) error {
	if value == nil {
		*dst = nil
		return nil
	}
	if v, ok := value.(int); ok {
		*dst = &v
		return nil
	}
	return fmt.Errorf("%w: %s expects int or nil, got %T", ErrInvalidPath, path, value)
}

func cloneStringMap(in map[string]string) map[string]string {
	return maps.Clone(in)
}

// recordRequestLocked appends a RequestRecord. The caller holds mu.
func (s *state) recordRequestLocked(rec RequestRecord) {
	s.requests = append(s.requests, rec)
}

// snapshotRequestsLocked returns a deep copy of the audit slice. The
// caller holds mu.
func (s *state) snapshotRequestsLocked() []RequestRecord {
	out := make([]RequestRecord, len(s.requests))
	copy(out, s.requests)
	return out
}
