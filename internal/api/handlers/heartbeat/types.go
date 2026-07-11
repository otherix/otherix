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
	Blobs        []blobReport           `json:"blobs,omitempty"`
	// BlobsUnavailable is true when the agent could not enumerate its artifact
	// store this tick (a transient List error). The CP then preserves the prior
	// node_blobs inventory rather than clearing it (fail-closed, mirrors
	// ImagesUnavailable).
	BlobsUnavailable bool `json:"blobs_unavailable,omitempty"`
	// ImageBlobs is the agent's reported pinned-image cache inventory (the
	// peer-pullable image tier). Reported on a SEPARATE channel from Blobs and
	// persisted to the image_blobs family so it never feeds the durability
	// reconcile.
	ImageBlobs []blobReport `json:"image_blobs,omitempty"`
	// ImageBlobsUnavailable mirrors BlobsUnavailable for the image tier: true
	// when the agent could not enumerate its image cache this tick. The CP then
	// preserves the prior image_blobs inventory rather than clearing it.
	ImageBlobsUnavailable bool `json:"image_blobs_unavailable,omitempty"`
	// HealthChecks carries the agent's active L4 probe verdicts for the load
	// balancer backends declared on this node (the observed-health up-channel).
	// The CP folds each verdict into the backend's observed health.
	HealthChecks []healthCheckReport `json:"health_checks,omitempty"`
	// PublishedListeners carries a gateway node's observed bind state for each
	// published load balancer listener it was declared. The CP folds each entry
	// into the observed lb_published_listener_status keyed by (lb_id, reporting
	// node). Non-empty only from a gateway recipient.
	PublishedListeners []publishedListenerReport `json:"published_listeners,omitempty"`
	// IngressAdvertisedEndpoint is the node's self-reported ingress splicer URL
	// (/v1/connect), reported when the node serves the ingress plane. The CP folds
	// it into node.IngressAdvertisedEndpoint preserve-on-empty: a non-empty value
	// overwrites, an empty/absent value leaves the stored endpoint untouched so a
	// transient empty tick never drops the node out of gateway selection.
	IngressAdvertisedEndpoint string `json:"ingress_advertised_endpoint,omitempty"`
}

// healthCheckReport mirrors HealthCheckReport on the agent side (the manual-sync
// contract) — one active L4 probe verdict the agent reports up-channel for a
// load balancer backend. Healthy is the agent's current verdict for probing
// VMID as a backend of LBID.
type healthCheckReport struct {
	LBID    uuid.UUID `json:"lb_id"`
	VMID    uuid.UUID `json:"vm_id"`
	Healthy bool      `json:"healthy"`
}

// publishedListenerReport mirrors PublishedListenerReport on the agent side (the
// manual-sync contract) — one gateway node's observed bind state for a published
// load balancer's public listener. Bound is the agent's verdict for its last bind
// attempt; Error carries the failure string when Bound is false.
type publishedListenerReport struct {
	LBID  uuid.UUID `json:"lb_id"`
	Port  int32     `json:"port"`
	Bound bool      `json:"bound"`
	Error string    `json:"error,omitempty"`
}

// blobReport mirrors HeartbeatBlob (the agent up-channel node-level blob entry).
// The CP persists the per-node list as the observed node_blobs inventory, the
// holder-discovery source for the cross-node pull saga. Identity is
// the digest; SizeBytes is observability.
type blobReport struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
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
	// ImagesUnavailable is true when the agent could not enumerate this pool's
	// image cache this tick (a transient ListImages error). The CP then
	// preserves the prior inventory rather than clearing it.
	ImagesUnavailable bool                 `json:"images_unavailable,omitempty"`
	Snapshots         []poolSnapshotReport `json:"snapshots,omitempty"`
	// SnapshotsUnavailable mirrors ImagesUnavailable for the snapshot blob
	// inventory. Accepted but NOT yet consumed CP-side (the agent only
	// reports it; observed-state projection lands later). The field is
	// declared here so DisallowUnknownFields does not 400 an upgraded agent's
	// heartbeat.
	SnapshotsUnavailable bool `json:"snapshots_unavailable,omitempty"`
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

// poolSnapshotReport mirrors one entry of HeartbeatPoolReport.snapshots — the
// agent's observed per-pool snapshot-blob cache (content-addressed). Accepted
// but NOT yet projected onto observed state; declared
// here so DisallowUnknownFields accepts an upgraded agent's heartbeat. Mirrors
// poolImageReport.
type poolSnapshotReport struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
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
	// ActiveSessions is a gateway's self-reported count of live ingress sessions
	// on this network. Zero/absent for a hypervisor node. The CP stores it on the
	// network_node_status row, where the gateway coverage reconcile reads it to
	// keep a membership sticky while sessions drain.
	ActiveSessions int `json:"active_sessions,omitempty"`
}

type migrationCapability struct {
	Host           string `json:"host"`
	PortRangeStart int32  `json:"port_range_start"`
	PortRangeEnd   int32  `json:"port_range_end"`
}

type nodeCapabilitiesReport struct {
	CPUModel           string                `json:"cpu_model"`
	CPUFlags           []string              `json:"cpu_flags"`
	CPUCoresTotal      int32                 `json:"cpu_cores_total"`
	MemoryTotalMib     int64                 `json:"memory_total_mib"`
	Hugepages2MibTotal *int32                `json:"hugepages_2mib_total"`
	Hugepages1GibTotal *int32                `json:"hugepages_1gib_total"`
	KernelVersion      string                `json:"kernel_version"`
	QEMUVersion        string                `json:"qemu_version"`
	KvmAvailable       bool                  `json:"kvm_available"`
	NestedVirt         bool                  `json:"nested_virt"`
	QEMUBinaries       map[string]string     `json:"qemu_binaries"`
	NumaTopology       map[string]any        `json:"numa_topology,omitempty"`
	CompressedSwap     *compressedSwapReport `json:"compressed_swap,omitempty"`
}

// compressedSwapReport mirrors the agent's CompressedSwapInfo. Nil means the
// node's compressed-swap net is off.
type compressedSwapReport struct {
	Kind        string `json:"kind"`
	SizeMib     int64  `json:"size_mib"`
	MemLimitMib int64  `json:"mem_limit_mib"`
	Algorithm   string `json:"algorithm"`
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

type vmReport struct {
	VMUUID             uuid.UUID `json:"vm_uuid"`
	Phase              string    `json:"phase"`
	ObservedGeneration *int64    `json:"observed_generation"`
	QEMUPID            *int32    `json:"qemu_pid"`
	LastStartedAt      *string   `json:"last_started_at"`
	LastErrorMessage   *string   `json:"last_error_message"`
	MemoryUsedMib      *int64    `json:"memory_used_mib"`
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
	// SessionCAPublicPEM is the PEM-encoded public half of the cluster
	// ingress-session CA, distributed to gateways so they verify short-lived
	// session credentials offline. Nil until the CA is provisioned; the private
	// half never leaves the control plane.
	SessionCAPublicPEM *string `json:"session_ca_public_pem,omitempty"`
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
	// DeclaredHealthChecks is the CP-declared set of active L4 health probes the
	// agent must run against load balancer backends placed on this node (the
	// desired-probe down-channel; the agent reports each verdict back up via
	// requestBody.HealthChecks). Full-snapshot semantics.
	DeclaredHealthChecks []declaredHealthCheck `json:"declared_health_checks"`
	// DeclaredLoadBalancers is the CP-declared set of published load balancers a
	// gateway-role node must bind a public listener for, each with its resolved
	// eligible backend set. Non-empty only for a gateway recipient; a hypervisor
	// node receives an empty list. Full-snapshot semantics: the agent binds
	// exactly the published ports in this list and closes the rest.
	DeclaredLoadBalancers []declaredLoadBalancer `json:"declared_load_balancers"`
}

// declaredLoadBalancer mirrors DeclaredLoadBalancer on the agent side (the
// manual-sync contract) — one published load balancer the CP wants a gateway to
// bind a public L4 listener for. PublishedPort is the public listener port,
// BackendPort the guest port each backend is dialed at, and SourceCIDRs the
// optional allowlist (empty means allow-all). Backends is the resolved eligible
// backend set (fail toward inclusion), node-independent across gateways.
type declaredLoadBalancer struct {
	LBID          uuid.UUID         `json:"lb_id"`
	PublishedPort int32             `json:"published_port"`
	Protocol      string            `json:"protocol"`
	BackendPort   int32             `json:"backend_port"`
	SourceCIDRs   []string          `json:"source_cidrs,omitempty"`
	Backends      []declaredBackend `json:"backends"`
}

// declaredBackend mirrors DeclaredBackend on the agent side — one resolved
// backend of a published load balancer: the backend VM plus the overlay address
// a gateway dials it at (OverlayIP is netip.Addr.String(), MAC is
// net.HardwareAddr.String()). Healthy is the observed active-health verdict,
// informational only — eligibility is already applied, so a backend appears here
// iff it is eligible.
type declaredBackend struct {
	VMID      uuid.UUID `json:"vm_id"`
	OverlayIP string    `json:"overlay_ip"`
	MAC       string    `json:"mac"`
	Healthy   bool      `json:"healthy"`
}

// declaredHealthCheck mirrors DeclaredHealthCheck on the agent side (the
// manual-sync contract) — one active L4 probe the CP wants the agent to run
// against a load balancer backend (the desired-probe down-channel). Port is the
// backend TCP port; the *Seconds fields pace each probe;
// HealthyThreshold/UnhealthyThreshold are the consecutive-probe counts that flip
// the reported verdict.
type declaredHealthCheck struct {
	VMID               uuid.UUID `json:"vm_id"`
	VMName             string    `json:"vm_name"`
	LBID               uuid.UUID `json:"lb_id"`
	Port               int32     `json:"port"`
	IntervalSeconds    int32     `json:"interval_seconds"`
	TimeoutSeconds     int32     `json:"timeout_seconds"`
	HealthyThreshold   int32     `json:"healthy_threshold"`
	UnhealthyThreshold int32     `json:"unhealthy_threshold"`
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
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	Managed      bool              `json:"managed"`
	Egress       string            `json:"egress"`
	BridgeName   string            `json:"bridge_name"`
	Mtu          int32             `json:"mtu"`
	VNI          *int32            `json:"vni"`
	Subnet       *string           `json:"subnet"`
	Gateway      *string           `json:"gateway"`
	DhcpEnabled  bool              `json:"dhcp"`
	DNSEnabled   bool              `json:"dns"`
	Reservations []dhcpReservation `json:"reservations"`
	// GatewayAddr carries the tenant IP + unicast MAC an ingress gateway must own
	// on this overlay bridge. Set only when the heartbeating node is a gateway
	// and holds a membership on the network; nil for a hypervisor node.
	GatewayAddr *gatewayAddr `json:"gateway_addr"`
}

// gatewayAddr is the tenant-facing address an ingress gateway owns on an overlay
// bridge: IP is the tenant IP the gateway host originates and answers at, and
// MAC is the distinct unicast hardware address the bridge claims so return
// traffic to the MAC advertised in the synthetic overlay FDB placement is
// delivered locally. Mirrors GatewayAddr on the agent side.
type gatewayAddr struct {
	IP  string `json:"ip"`
	MAC string `json:"mac"`
}

// dhcpReservation mirrors DhcpReservation on the agent side (the manual-sync
// contract). One CP-IPAM MAC->IP binding the per-node DHCP responder serves;
// non-empty only for dhcp-enabled networks.
type dhcpReservation struct {
	MAC string `json:"mac"`
	IP  string `json:"ip"`
}
