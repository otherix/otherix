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
// two declarations in sync is a manual contract obligation. Drift
// surfaces as a 400 validation_failed on the receiver.
type Report struct {
	AgentVersion string           `json:"agent_version"`
	Architecture string           `json:"architecture"`
	Migration    *MigrationCap    `json:"migration,omitempty"`
	Capabilities NodeCapabilities `json:"capabilities"`
	Resources    NodeResources    `json:"resources"`
	VMs          []VMReport       `json:"vms"`
	Pools        []PoolReport     `json:"pools,omitempty"`
	Networks     []NetworkReport  `json:"networks,omitempty"`
	WireGuard    *WireGuardReport `json:"wireguard,omitempty"`
}

// WireGuardReport is the agent's observed WG interface state (the heartbeat
// up-channel). PublicKey + Endpoint are authoritative for CP redistribution;
// ListenPort + EstablishedPeers are observability. ReconciliationStatus
// (pending/ready/failed) + ReconciliationError surface the outcome of the WG
// reconciler's last pass so an otwg0 failure is visible like a bridge failure.
type WireGuardReport struct {
	PublicKey            string   `json:"public_key"`
	Endpoint             string   `json:"endpoint"`
	ListenPort           int32    `json:"listen_port"`
	EstablishedPeers     []string `json:"established_peers,omitempty"`
	ReconciliationStatus string   `json:"reconciliation_status"`
	ReconciliationError  *string  `json:"reconciliation_error"`
}

// DeclaredWireGuardPeer is one other agent the CP wants in this agent's WG mesh
// (the heartbeat down-channel). AllowedIPs carries the peer's overlay /32.
type DeclaredWireGuardPeer struct {
	NodeID     string   `json:"node_id"`
	PublicKey  string   `json:"public_key"`
	Endpoint   string   `json:"endpoint"`
	OverlayIP  string   `json:"overlay_ip"`
	AllowedIPs []string `json:"allowed_ips"`
}

// DeclaredFDBEntry is one controller-programmed VXLAN FDB entry the CP wants in
// this node's otvx<vni> kernel FDB (the heartbeat down-channel). A normal MAC is
// a per-VM unicast entry (mac -> the remote VM's owning-node VTEP); the all-zeros
// MAC "00:00:00:00:00:00" is a BUM/flood entry (head-end replication to that
// remote VTEP). VtepIP is the remote node's otwg0 overlay host IP.
type DeclaredFDBEntry struct {
	VNI    int32  `json:"vni"`
	MAC    string `json:"mac"`
	VtepIP string `json:"vtep_ip"`
}

// PoolReport mirrors HeartbeatPoolReport — one entry per pool the
// agent has observed after a reconciliation pass. Forward-compatibility
// capacity fields (capacity_bytes / available_bytes / reported_at) are
// omitted from this struct intentionally; capacity reporting stays on
// `storage_pool.scan` for this iteration. Add them when the
// scan-subsumption iteration lands.
type PoolReport struct {
	Name                 string            `json:"name"`
	ReconciliationStatus string            `json:"reconciliation_status"`
	ReconciliationError  *string           `json:"reconciliation_error,omitempty"`
	Images               []PoolImageReport `json:"images,omitempty"`
}

// PoolImageReport is one cached image the agent observed in a pool's image
// directory (basename-keyed cache). Carried inside PoolReport.Images; the CP
// stores it as observed pool state and surfaces it through pool get.
type PoolImageReport struct {
	Basename         string `json:"basename"`
	SHA256           string `json:"sha256"`
	SizeBytes        int64  `json:"size_bytes"`
	VirtualSizeBytes int64  `json:"virtual_size_bytes"`
	Format           string `json:"format"`
	ImportedAt       string `json:"imported_at"`
}

// NetworkReport mirrors HeartbeatNetworkReport — one entry per network
// the agent has observed after a reconciliation pass. Networks are
// cluster-wide, so the report keys on the network id (uuid), unlike
// PoolReport which keys on the node-scoped pool name. The CP upserts
// the (network_id, node_id) status record from each entry.
type NetworkReport struct {
	ID                   string  `json:"id"`
	ReconciliationStatus string  `json:"reconciliation_status"`
	ReconciliationError  *string `json:"reconciliation_error,omitempty"`
}

// Response mirrors HeartbeatResponse. The CP returns the desired
// pool inventory (declared_pools) on every heartbeat so the agent
// reconciler keeps its desired-state cache fresh without requiring a
// separate channel. The same shape extends to VMs: declared_vms
// carries the CP-declared per-node desired-state VM set (name +
// desired_phase + generation) so the agent's VM reconciler can diff
// observed vs declared and apply corrective lifecycle ops.
type Response struct {
	ReceivedAt             string                  `json:"received_at"`
	DeclaredPools          []DeclaredPool          `json:"declared_pools"`
	DeclaredVMs            []DeclaredVM            `json:"declared_vms"`
	DeclaredNetworks       []DeclaredNetwork       `json:"declared_networks"`
	DeclaredWireGuardPeers []DeclaredWireGuardPeer `json:"declared_wireguard_peers"`
	SelfOverlayIP          *string                 `json:"self_overlay_ip"`
	DeclaredFDB            []DeclaredFDBEntry      `json:"declared_fdb"`
	// Otwg0MTU is the CP-declared otwg0 link MTU (underlay - WGEncapOverhead).
	// Nil from an older CP or before the underlay MTU is known; the WG
	// reconciler falls back to netfabric.WireGuardMTU when absent.
	Otwg0MTU *int32 `json:"otwg0_mtu"`
	// OverlayReachability is the per-VNI non-blocking reachability signal: how
	// many remote placements the CP omitted from DeclaredFDB because the owning
	// node has no overlay IP yet. Observability only; the agent never gates the
	// overlay on it, converging on the (smaller) programmable FDB set it does
	// receive.
	OverlayReachability []OverlayReachability `json:"overlay_reachability,omitempty"`
}

// OverlayReachability is the per-VNI non-blocking reachability signal the CP
// reports down-channel. SkippedNoIP counts remote placements dropped from
// DeclaredFDB for that VNI because the owning node lacks an overlay IP.
// EstablishedPeers/TotalPeers report how many of the VNI's distinct flood-target
// VTEPs the reporting node currently has an established WireGuard handshake with —
// a flood target up in the FDB but with no live tunnel still blackholes BUM
// traffic. The agent surfaces these as signals (a remote VM unreachable until its
// node gets an overlay IP, or until its tunnel establishes) without ever holding
// the overlay at pending — per-peer reachability is non-blocking.
type OverlayReachability struct {
	VNI              int32 `json:"vni"`
	SkippedNoIP      int32 `json:"skipped_no_ip"`
	EstablishedPeers int32 `json:"established_peers"`
	TotalPeers       int32 `json:"total_peers"`
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
// DesiredPhase + the live Manager observation and dispatches Start /
// Stop / Delete; Generation is the spec generation the agent must
// catch up to (observed_generation in vm_runtime).
type DeclaredVM struct {
	Name         string `json:"name"`
	DesiredPhase string `json:"desired_phase"`
	Generation   int64  `json:"generation"`
}

// DeclaredNetwork mirrors HeartbeatDeclaredNetwork — one network the CP
// wants materialised on this node. Networks are cluster-wide, so every
// node receives the same list. The agent reconciler diffs the declared
// set against its observed bridges and applies changes autonomously.
// Subnet (canonical CIDR) and Gateway (IP) are populated when
// Egress="nat", null otherwise. VNI is non-nil only for type=overlay.
type DeclaredNetwork struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	Managed    bool    `json:"managed"`
	Egress     string  `json:"egress"`
	BridgeName string  `json:"bridge_name"`
	Mtu        int32   `json:"mtu"`
	VNI        *int32  `json:"vni"`
	Subnet     *string `json:"subnet"`
	Gateway    *string `json:"gateway"`
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
// agent omits them when the syscall fails, and the CP receiver
// carries existing pressure state forward on NULL input.
type NodeResources struct {
	CPUCoresAvailable        int32  `json:"cpu_cores_available"`
	MemoryAvailableMib       int64  `json:"memory_available_mib"`
	SystemDiskTotalBytes     *int64 `json:"system_disk_total_bytes,omitempty"`
	SystemDiskAvailableBytes *int64 `json:"system_disk_available_bytes,omitempty"`
}

// FirmwareReport describes one firmware blob the agent has on disk.
// The CP joins on (name, architecture, type) to firmwares.id and upserts
// the resulting node_firmware row.
type FirmwareReport struct {
	Name             string  `json:"name"`
	Architecture     string  `json:"architecture"`
	Type             string  `json:"type"`
	CodePath         string  `json:"code_path"`
	VarsTemplatePath *string `json:"vars_template_path"`
	SecureBoot       bool    `json:"secure_boot"`
}

// VMReport is one entry in the per-VM runtime list. The CP joins on
// vm_uuid and updates vm_runtime; absent VMs are reconciled as missing
// on the node (full-snapshot semantics).
type VMReport struct {
	VMUUID             uuid.UUID `json:"vm_uuid"`
	Phase              string    `json:"phase"`
	ObservedGeneration *int64    `json:"observed_generation"`
	QEMUPID            *int32    `json:"qemu_pid"`
	LastStartedAt      *string   `json:"last_started_at"`
	LastErrorMessage   *string   `json:"last_error_message"`
}
