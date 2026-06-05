// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import "github.com/google/uuid"

// requestBody mirrors HeartbeatRequest in
// api/openapi/control-plane.yaml. It is hand-written rather than
// codegen-driven because the CP side does not run through codegen.
// Field shapes follow the spec exactly; nullable spec fields land
// as Go pointers.
type requestBody struct {
	AgentVersion string                 `json:"agent_version"`
	Architecture string                 `json:"architecture"`
	Migration    *migrationCapability   `json:"migration,omitempty"`
	Capabilities nodeCapabilitiesReport `json:"capabilities"`
	Resources    nodeResourcesReport    `json:"resources"`
	VMs          []vmReport             `json:"vms"`
	Pools        []poolReport           `json:"pools,omitempty"`
	Networks     []networkReport        `json:"networks"`
	WireGuard    *wireGuardReport       `json:"wireguard,omitempty"`
}

// wireGuardReport mirrors the agent's observed WG state. public_key + endpoint
// are authoritative for redistribution; listen_port + established_peers are
// observability. A nil report (agent without WG state) is skipped at ingest.
type wireGuardReport struct {
	PublicKey            string   `json:"public_key"`
	Endpoint             string   `json:"endpoint"`
	ListenPort           int32    `json:"listen_port"`
	EstablishedPeers     []string `json:"established_peers,omitempty"`
	ReconciliationStatus string   `json:"reconciliation_status"`
	ReconciliationError  *string  `json:"reconciliation_error"`
}

// declaredWireGuardPeer is one peer in the fabric down-channel: another agent's
// CP-assigned identity (node_id, pubkey, endpoint, overlay_ip) plus the peer's
// overlay /32 as allowed_ips.
type declaredWireGuardPeer struct {
	NodeID     string   `json:"node_id"`
	PublicKey  string   `json:"public_key"`
	Endpoint   string   `json:"endpoint"`
	OverlayIP  string   `json:"overlay_ip"`
	AllowedIPs []string `json:"allowed_ips"`
}

// declaredFDBEntry mirrors DeclaredFDBEntry on the agent side (the manual-sync
// contract). The all-zeros MAC is the BUM/flood entry.
type declaredFDBEntry struct {
	VNI    int32  `json:"vni"`
	MAC    string `json:"mac"`
	VtepIP string `json:"vtep_ip"`
}

// poolReport mirrors HeartbeatPoolReport — agent's per-pool
// reconciliation outcome. The CP joins each entry against
// `storage_pools` by (node_id, lower(name)) and applies
// `reconciliation_status` / `reconciliation_error` to the matched row.
// Capacity / availability fields exist on the OpenAPI side for
// forward-compatibility but are not deserialised
// here yet — scan-driven capacity reporting remains canonical.
type poolReport struct {
	Name                 string            `json:"name"`
	ReconciliationStatus string            `json:"reconciliation_status"`
	ReconciliationError  *string           `json:"reconciliation_error,omitempty"`
	Images               []poolImageReport `json:"images,omitempty"`
}

// poolImageReport mirrors one entry of HeartbeatPoolReport.images — the agent's
// observed per-pool image cache. The CP keys the inventory on the pool's UUID
// (resolved from (node_id, lower(name))) and persists it as observed state.
// ImportedAt is an agent-supplied RFC3339 string; a parse failure degrades to
// zero time rather than dropping the image (identity is basename+sha256).
type poolImageReport struct {
	Basename         string `json:"basename"`
	SHA256           string `json:"sha256"`
	SizeBytes        int64  `json:"size_bytes"`
	VirtualSizeBytes int64  `json:"virtual_size_bytes"`
	Format           string `json:"format"`
	ImportedAt       string `json:"imported_at"`
}

// networkReport mirrors HeartbeatNetworkReport — the agent's per-network
// reconciliation outcome. Networks are cluster-wide, so the report keys on
// the network id (uuid) rather than a node-scoped name; the CP parses ID
// and upserts the (network_id, node_id) status record. A non-uuid ID is
// tolerated (logged + skipped) so a stale or garbage report cannot 500 the
// whole heartbeat.
type networkReport struct {
	ID                   string  `json:"id"`
	ReconciliationStatus string  `json:"reconciliation_status"`
	ReconciliationError  *string `json:"reconciliation_error"`
}

type migrationCapability struct {
	Host           string `json:"host"`
	PortRangeStart int32  `json:"port_range_start"`
	PortRangeEnd   int32  `json:"port_range_end"`
}

type nodeCapabilitiesReport struct {
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
	Firmwares          []firmwareReport  `json:"firmwares"`
}

type nodeResourcesReport struct {
	CPUCoresAvailable  int32 `json:"cpu_cores_available"`
	MemoryAvailableMib int64 `json:"memory_available_mib"`
	// Root filesystem metrics for system_disk
	// pressure detection. Both nullable: the agent reports them when
	// statfs("/") succeeds, omits when the syscall fails. The CP holds
	// last-known values across heartbeat gaps and the pressure
	// transition function silently carries state forward on NULL input.
	SystemDiskTotalBytes     *int64 `json:"system_disk_total_bytes,omitempty"`
	SystemDiskAvailableBytes *int64 `json:"system_disk_available_bytes,omitempty"`
}

type firmwareReport struct {
	Name             string  `json:"name"`
	Architecture     string  `json:"architecture"`
	Type             string  `json:"type"`
	CodePath         string  `json:"code_path"`
	VarsTemplatePath *string `json:"vars_template_path"`
	SecureBoot       bool    `json:"secure_boot"`
}

type vmReport struct {
	VMUUID             uuid.UUID `json:"vm_uuid"`
	Phase              string    `json:"phase"`
	ObservedGeneration *int64    `json:"observed_generation"`
	QEMUPID            *int32    `json:"qemu_pid"`
	LastStartedAt      *string   `json:"last_started_at"`
	LastErrorMessage   *string   `json:"last_error_message"`
}

// responseBody mirrors HeartbeatResponse — acknowledgement plus
// desired-state pool inventory. The agent reconciler
// replaces its desired-state cache from declared_pools every heartbeat.
// declared_vms follows the same pattern for VMs.
type responseBody struct {
	ReceivedAt             string                  `json:"received_at"`
	DeclaredPools          []declaredPool          `json:"declared_pools"`
	DeclaredVMs            []declaredVM            `json:"declared_vms"`
	DeclaredNetworks       []declaredNetwork       `json:"declared_networks"`
	DeclaredWireGuardPeers []declaredWireGuardPeer `json:"declared_wireguard_peers"`
	SelfOverlayIP          *string                 `json:"self_overlay_ip"`
	DeclaredFDB            []declaredFDBEntry      `json:"declared_fdb"`
	// Otwg0MTU is the CP-declared otwg0 link MTU (underlay - WGEncapOverhead),
	// mirrored to the agent so it brings otwg0 up at the right size on a
	// sub-1500 underlay.
	Otwg0MTU *int32 `json:"otwg0_mtu"`
	// OverlayReachability carries the per-VNI non-blocking reachability signal:
	// how many remote placements the CP had to omit from declared_fdb because
	// their owning node has no overlay IP yet. It is observability only and
	// never gates readiness — the agent converges on the (smaller) programmable
	// FDB set regardless.
	OverlayReachability []overlayReachability `json:"overlay_reachability,omitempty"`
}

// overlayReachability mirrors OverlayReachability on the agent side — the per-VNI
// non-blocking reachability signal for CP-declared FDB. SkippedNoIP is the count
// of remote placements dropped from declared_fdb for that VNI because the owning
// node lacks an overlay IP. EstablishedPeers/TotalPeers report how many of the
// VNI's distinct flood-target VTEPs (each a remote node with an overlay IP) the
// reporting node currently has an established WireGuard handshake with: a flood
// target that is up in the FDB but has no live tunnel still blackholes BUM
// traffic, so established_peers < total_peers is a false-ready signal. Both are
// observability only and never hold the overlay pending — gating on them would
// reintroduce the C4 wedge and flap on every WireGuard rekey.
type overlayReachability struct {
	VNI              int32 `json:"vni"`
	SkippedNoIP      int32 `json:"skipped_no_ip"`
	EstablishedPeers int32 `json:"established_peers"`
	TotalPeers       int32 `json:"total_peers"`
}

// declaredPool mirrors HeartbeatDeclaredPool — one entry per pool the
// CP wants materialised on this node. Order is stable (lower(name)
// asc) so the agent's diff stays deterministic across heartbeats.
type declaredPool struct {
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Path   string         `json:"path"`
	Config map[string]any `json:"config,omitempty"`
}

// declaredVM mirrors HeartbeatDeclaredVM — desired-state record per
// VM the CP wants converged on this node. Order is stable
// (lower(name) asc) per the same determinism contract as
// declared_pools. `Generation` is the spec generation the agent
// records into vm_runtime.observed_generation as it converges.
type declaredVM struct {
	Name         string `json:"name"`
	DesiredPhase string `json:"desired_phase"`
	Generation   int64  `json:"generation"`
}

// declaredNetwork mirrors HeartbeatDeclaredNetwork — one network the CP
// wants materialised on this node. Networks are cluster-wide, so every
// node receives the same set; order is stable (by id) so the agent's
// diff stays deterministic across heartbeats. Subnet (canonical CIDR)
// and Gateway (IP) are non-nil only when Egress="nat". VNI is non-nil
// only for type=overlay.
type declaredNetwork struct {
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
