// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import "github.com/google/uuid"

// Report mirrors HeartbeatRequest in api/openapi/control-plane.yaml.
// Hand-written rather than codegen-driven because the agent does not
// run the oapi-codegen pipeline today. Field shapes follow the spec
// exactly; nullable spec fields land as Go pointers.
//
// The CP-side receiver (internal/api/handlers/heartbeat/types.go)
// has the symmetric type with the same JSON tags — keeping these
// two declarations in sync is а manual contract obligation. Drift
// surfaces as а 400 validation_failed на the receiver.
type Report struct {
	AgentVersion string           `json:"agent_version"`
	Architecture string           `json:"architecture"`
	Migration    *MigrationCap    `json:"migration,omitempty"`
	Capabilities NodeCapabilities `json:"capabilities"`
	Resources    NodeResources    `json:"resources"`
	VMs          []VMReport       `json:"vms"`
	Pools        []PoolReport     `json:"pools,omitempty"`
}

// PoolReport mirrors HeartbeatPoolReport — one entry per pool the
// agent has observed after a reconciliation pass. Forward-compatibility
// capacity fields (capacity_bytes / available_bytes / reported_at) are
// omitted from this struct intentionally; capacity reporting stays on
// `storage_pool.scan` for this iteration. Add them when the
// scan-subsumption iteration lands.
type PoolReport struct {
	Name                 string  `json:"name"`
	ReconciliationStatus string  `json:"reconciliation_status"`
	ReconciliationError  *string `json:"reconciliation_error,omitempty"`
}

// Response mirrors HeartbeatResponse. The CP returns the desired
// pool inventory (declared_pools) on every heartbeat so the agent
// reconciler keeps its desired-state cache fresh без requiring а
// separate channel. The same shape extends к VMs: declared_vms
// carries the CP-declared per-node desired-state VM set (name +
// desired_phase + generation) so the agent's VM reconciler can diff
// observed vs declared and apply corrective lifecycle ops.
type Response struct {
	ReceivedAt    string         `json:"received_at"`
	DeclaredPools []DeclaredPool `json:"declared_pools"`
	DeclaredVMs   []DeclaredVM   `json:"declared_vms"`
}

// DeclaredPool mirrors HeartbeatDeclaredPool — one pool the CP wants
// materialised on this node. The agent reconciler diffs the latest
// declared_pools list against its observed registry and applies
// changes autonomously (mkdir on new, unregister on removed).
type DeclaredPool struct {
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Path   string         `json:"path"`
	Config map[string]any `json:"config,omitempty"`
}

// DeclaredVM mirrors HeartbeatDeclaredVM — desired-state record per
// VM the CP wants converged on this node. The VM reconciler reads
// DesiredPhase + the live Manager observation и dispatches Start /
// Stop / Delete; Generation is the spec generation the agent must
// catch up к (observed_generation в vm_runtime).
type DeclaredVM struct {
	Name         string `json:"name"`
	DesiredPhase string `json:"desired_phase"`
	Generation   int64  `json:"generation"`
}

// MigrationCap advertises the migration ingress configuration. The
// CP rewrites nodes.migration_host / port_range_* whenever this
// block is present.
type MigrationCap struct {
	Host           string `json:"host"`
	PortRangeStart int32  `json:"port_range_start"`
	PortRangeEnd   int32  `json:"port_range_end"`
}

// NodeCapabilities is the slow-moving host inventory: CPU model,
// kernel/qemu versions, NUMA topology, firmware catalogue.
type NodeCapabilities struct {
	CPUModel           string            `json:"cpu_model"`
	CPUFlags           []string          `json:"cpu_flags"`
	CPUCoresTotal      int32             `json:"cpu_cores_total"`
	MemoryTotalMib     int64             `json:"memory_total_mib"`
	Hugepages2MibTotal *int32            `json:"hugepages_2mib_total"`
	Hugepages1GibTotal *int32            `json:"hugepages_1gib_total"`
	KernelVersion      string            `json:"kernel_version"`
	QEMUVersion        string            `json:"qemu_version"`
	KvmAvailable       bool              `json:"kvm_available"`
	NestedVirt         bool              `json:"nested_virt"`
	QEMUBinaries       map[string]string `json:"qemu_binaries"`
	NumaTopology       map[string]any    `json:"numa_topology,omitempty"`
	Firmwares          []FirmwareReport  `json:"firmwares"`
}

// NodeResources is the per-tick free-resource snapshot. Free is
// computed by subtracting running VM allocations from host totals;
// negative values are clamped to zero before serialisation.
//
// SystemDiskTotalBytes / SystemDiskAvailableBytes carry root
// filesystem capacity from syscall.Statfs("/"). Both nullable: the
// agent omits them когда the syscall fails, и the CP receiver
// carries existing pressure state forward on NULL input.
type NodeResources struct {
	CPUCoresAvailable        int32  `json:"cpu_cores_available"`
	MemoryAvailableMib       int64  `json:"memory_available_mib"`
	SystemDiskTotalBytes     *int64 `json:"system_disk_total_bytes,omitempty"`
	SystemDiskAvailableBytes *int64 `json:"system_disk_available_bytes,omitempty"`
}

// FirmwareReport describes one firmware blob the agent has on disk.
// The CP joins on (name, architecture, type) к firmwares.id и upserts
// the resulting node_firmware row.
type FirmwareReport struct {
	Name             string  `json:"name"`
	Architecture     string  `json:"architecture"`
	Type             string  `json:"type"`
	CodePath         string  `json:"code_path"`
	VarsTemplatePath *string `json:"vars_template_path"`
	SecureBoot       bool    `json:"secure_boot"`
}

// VMReport is one entry в the per-VM runtime list. The CP joins on
// vm_uuid и updates vm_runtime; absent VMs are reconciled as missing
// on the node (full-snapshot semantics).
type VMReport struct {
	VMUUID             uuid.UUID `json:"vm_uuid"`
	Phase              string    `json:"phase"`
	ObservedGeneration *int64    `json:"observed_generation"`
	QEMUPID            *int32    `json:"qemu_pid"`
	LastStartedAt      *string   `json:"last_started_at"`
	LastErrorMessage   *string   `json:"last_error_message"`
}
