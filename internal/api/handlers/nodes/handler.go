// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package nodes hosts the /v1/nodes/* HTTP handlers. The CRUD surface
// (create, list, get, delete) plus the cordon / uncordon maintenance
// toggles is gated by `node:read`, `node:maintenance`, and `node:manage`
// per docs/rbac.md. Manual node creation pre-registers a row so an agent
// can later attach via mTLS join-token (which is a separate slice).
package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the nodes handlers depend on: the node
// domain methods plus the identifier-resolution contract
// (resolver.Querier) the handlers use to resolve the name-only {id}
// path parameter. Depending on the interface rather than the concrete
// *etcdstore.Store narrows the handler's storage dependency to the methods
// it uses and lets tests substitute a fake. *etcdstore.Store satisfies it.
type Store interface {
	resolver.Querier

	NodeEffectiveByID(ctx context.Context, id uuid.UUID) (store.NodeEffectiveAvailability, error)
	CreateNode(ctx context.Context, arg store.CreateNodeParams) (store.Node, error)
	CordonNode(ctx context.Context, id uuid.UUID) (store.Node, error)
	UncordonNode(ctx context.Context, id uuid.UUID) (store.Node, error)
	SetNodeGatewayRole(ctx context.Context, id uuid.UUID, enabled bool) (store.Node, error)
	ReadmitNode(ctx context.Context, id uuid.UUID) (store.Node, error)
	ListNodesEffective(ctx context.Context, arg store.ListNodesEffectiveParams) ([]store.NodeEffectiveAvailability, error)
	DeleteNode(ctx context.Context, id uuid.UUID, force bool, callerID uuid.UUID) (store.NodeDeleteOutcome, error)
	ListNetworkNodeStatusByNode(ctx context.Context, nodeID uuid.UUID) ([]store.NetworkNodeStatus, error)
	NetworkByID(ctx context.Context, id uuid.UUID) (store.Network, error)
	AgentWireguardByNodeID(ctx context.Context, nodeID uuid.UUID) (store.AgentWireguard, error)
	ListAgentWireguard(ctx context.Context) ([]store.AgentWireguard, error)
	NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error)
	NodeIDsWithPool(ctx context.Context) (map[uuid.UUID]struct{}, error)

	// Node-drain surface. The HTTP Drain handler uses StartNodeDrain (atomic
	// node-flip + task + job enqueue), the cluster-default timeout / concurrency
	// reads, and TaskByID to replay the in-flight task on an idempotent repeat.
	// The remaining methods back the node.drain saga (the consumer that drives
	// the drain to completion) and are declared here so the handler package's
	// store contract stays the single interface the saga also depends on.
	StartNodeDrain(ctx context.Context, nodeID uuid.UUID, taskParams store.CreateTaskParams, args queue.JobArgs) (store.Task, error)
	CountDrainingNodes(ctx context.Context) (int, error)
	FinishNodeDrain(ctx context.Context, nodeID, taskID uuid.UUID, status store.TaskStatus, result []byte) error
	DrainTimeoutSeconds(ctx context.Context) (int32, error)
	DrainMaxConcurrentMigrations(ctx context.Context) (int32, error)
	TaskByID(ctx context.Context, id uuid.UUID) (store.Task, error)
	ListVMRefsForNodeDeclared(ctx context.Context, nodeID uuid.UUID) ([]store.NodeVMRef, error)
	ActiveSourceMigrationCount(ctx context.Context, nodeID uuid.UUID) (int, error)
	VMByID(ctx context.Context, id uuid.UUID) (store.VM, error)
	CreateMigration(ctx context.Context, p store.CreateMigrationParams, args queue.JobArgs) (store.Migration, error)
	UpdateTaskRunning(ctx context.Context, id uuid.UUID) (alreadyTerminal bool, err error)
	UpdateTaskFinalized(ctx context.Context, arg store.UpdateTaskFinalizedParams) error
	ListVMNicsByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMNic, error)
	DrainCancelRequested(ctx context.Context, taskID uuid.UUID) (bool, error)
}

// Handler bundles the dependencies for the nodes routes.
type Handler struct {
	store Store
	log   *slog.Logger
	// maxConcurrentDrains caps how many nodes may be draining at once. The Drain
	// handler rejects a new drain with 409 once this many nodes already are, so a
	// fleet drain cannot exhaust the worker pool. Always >= 1 after New clamps it.
	maxConcurrentDrains int
}

// New constructs a Handler. It takes the Store interface so any
// conforming backend can be wired in; production passes *store.Store.
// maxConcurrentDrains caps simultaneous node drains; a value < 1 falls back to
// config.DefaultMaxConcurrentDrains so a misconfiguration cannot disable the
// guard (0 would otherwise reject every drain).
func New(s Store, log *slog.Logger, maxConcurrentDrains int) *Handler {
	if maxConcurrentDrains < 1 {
		maxConcurrentDrains = config.DefaultMaxConcurrentDrains
	}
	return &Handler{store: s, log: log, maxConcurrentDrains: maxConcurrentDrains}
}

// nodeOwnsPool reports whether the node owns at least one non-deleted storage
// pool - the read-boundary hypervisor signal. One pool-keyspace scan; node
// reads are not a hot path.
func (h *Handler) nodeOwnsPool(ctx context.Context, id uuid.UUID) (bool, error) {
	set, err := h.store.NodeIDsWithPool(ctx)
	if err != nil {
		return false, err
	}
	_, ok := set[id]
	return ok, nil
}

// nodeView is the full Node projection returned to admin / operator
// callers. Mirrors the `Node` schema in api/openapi/control-plane.yaml.
//
// `cpu_cores_available` / `memory_available_mib` are the raw heartbeat-
// reported values. `cpu_cores_effective` / `memory_effective_mib` are
// the scheduler-relevant view that subtracts VMs pinned after the last
// heartbeat. Operators reading
// the two side-by-side can spot a race window — divergence implies a
// pending placement that the agent has not yet observed. Both pairs
// are nullable: NULL until the first heartbeat lands, and effective
// stays NULL whenever its raw counterpart is NULL.
//
// Endpoints that mutate node state (cordon / uncordon) return effective
// fields = nil — the post-mutation row is fetched via the raw nodes
// table for the RETURNING-row idiom, and effective values right after a
// state flip are not meaningful (a cordoned node will not receive new
// placements anyway).
type nodeView struct {
	ID                        string                 `json:"id"`
	Name                      string                 `json:"name"`
	Architecture              string                 `json:"architecture"`
	Roles                     []string               `json:"roles"`
	AdvertisedEndpoint        string                 `json:"advertised_endpoint"`
	IngressAdvertisedEndpoint string                 `json:"ingress_advertised_endpoint"`
	Migration                 migrationCap           `json:"migration"`
	Status                    string                 `json:"status"`
	CordonedAt                *string                `json:"cordoned_at"`
	CPUCoresTotal             *int32                 `json:"cpu_cores_total"`
	CPUCoresAvailable         *int32                 `json:"cpu_cores_available"`
	CPUCoresEffective         *int32                 `json:"cpu_cores_effective"`
	CPUModel                  *string                `json:"cpu_model"`
	CPUFlags                  []string               `json:"cpu_flags"`
	MemoryTotalMiB            *int64                 `json:"memory_total_mib"`
	MemoryAvailableMiB        *int64                 `json:"memory_available_mib"`
	MemoryEffectiveMiB        *int64                 `json:"memory_effective_mib"`
	Hugepages2MiB             *int32                 `json:"hugepages_2mib_total"`
	Hugepages1GiB             *int32                 `json:"hugepages_1gib_total"`
	KernelVersion             *string                `json:"kernel_version"`
	QEMUVersion               *string                `json:"qemu_version"`
	NumaTopology              json.RawMessage        `json:"numa_topology"`
	Capabilities              json.RawMessage        `json:"capabilities"`
	LastHeartbeatAt           *string                `json:"last_heartbeat_at"`
	AgentVersion              *string                `json:"agent_version"`
	Labels                    map[string]string      `json:"labels"`
	MemoryPressure            *pressureView          `json:"memory_pressure"`
	SystemDiskTotalBytes      *int64                 `json:"system_disk_total_bytes"`
	SystemDiskAvailableBytes  *int64                 `json:"system_disk_available_bytes"`
	SystemDiskPressure        *pressureView          `json:"system_disk_pressure"`
	CreatedAt                 string                 `json:"created_at"`
	UpdatedAt                 string                 `json:"updated_at"`
	NetworkConditions         []networkConditionView `json:"network_conditions"`
	WireGuard                 *wireguardView         `json:"wireguard"`
}

// networkConditionView is one per-(node, network) materialisation record
// projected for the full Node view. A networks row is one cluster-wide
// definition; materialising the bridge can succeed on some nodes and fail
// on others, so the per-node outcome surfaces here keyed by network. It is
// an admin / operator-only, GET-by-id detail: the list path leaves it nil
// to avoid a per-row fan-out over the status keyspace.
type networkConditionView struct {
	NetworkID            string  `json:"network_id"`
	Name                 string  `json:"name"`
	ReconciliationStatus string  `json:"reconciliation_status"`
	ReconciliationError  *string `json:"reconciliation_error"`
	LastReconciledAt     *string `json:"last_reconciled_at"`
}

// wireguardView is the node's WG underlay fabric block in the full Node view:
// the node's own agent_wireguard identity plus the mesh peer set with a
// per-peer established flag. admin/operator GET-by-id only (omitted on the list
// path and the summary view); nil when the agent has not reported WG state yet.
type wireguardView struct {
	OverlayIP            string              `json:"overlay_ip"`
	PublicKey            string              `json:"public_key"`
	ListenPort           int32               `json:"listen_port"`
	Endpoint             string              `json:"endpoint"`
	ReconciliationStatus string              `json:"reconciliation_status"`
	ReconciliationError  *string             `json:"reconciliation_error"`
	Peers                []wireguardPeerView `json:"peers"`
}

// wireguardPeerView is one other agent in this node's mesh. NodeName is
// best-effort (nil when the peer's node row was deleted; the CLI falls back to
// node_id). Established reflects this node's last observed handshake set.
type wireguardPeerView struct {
	NodeID      string  `json:"node_id"`
	NodeName    *string `json:"node_name"`
	OverlayIP   string  `json:"overlay_ip"`
	Established bool    `json:"established"`
}

// pressureView mirrors components/schemas/MemoryPressureCondition
// /SystemDiskPressureCondition. Both pressure types share
// the same wire shape so a single struct serves them; nullable on the
// wire — a node without active pressure has the parent field set to JSON
// null.
type pressureView struct {
	Since            string `json:"since"`
	ConsecutiveCount int32  `json:"consecutive_count"`
}

// nodeSummaryView is the reduced projection returned to developer /
// viewer callers. Mirrors the `NodeSummary` schema.
type nodeSummaryView struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Architecture string            `json:"architecture"`
	Roles        []string          `json:"roles"`
	Status       string            `json:"status"`
	CordonedAt   *string           `json:"cordoned_at"`
	Labels       map[string]string `json:"labels"`
	CreatedAt    string            `json:"created_at"`
}

// migrationCap mirrors components/schemas/MigrationCapability for the
// public API surface. Agent-side runtime fields (ports_in_use,
// ports_available) are NOT carried here — those come from the agent's
// /v1/info, not the CP DB.
type migrationCap struct {
	Host           string `json:"host"`
	PortRangeStart int32  `json:"port_range_start"`
	PortRangeEnd   int32  `json:"port_range_end"`
}

// toViewEffective builds the full nodeView from the view-backed row.
// Surfaces cpu_cores_effective / memory_effective_mib alongside the
// raw heartbeat columns. Used by GET /v1/nodes/{name}
// and GET /v1/nodes (list). ownsPool feeds the derived hypervisor role: a node
// that owns at least one storage pool is VM-schedulable.
func toViewEffective(n store.NodeEffectiveAvailability, ownsPool bool) nodeView {
	v := nodeView{
		ID:                        n.ID.String(),
		Name:                      n.Name,
		Architecture:              string(n.Architecture),
		Roles:                     store.EffectiveRoles(n.GatewayRole, ownsPool),
		AdvertisedEndpoint:        n.AdvertisedEndpoint,
		IngressAdvertisedEndpoint: n.IngressAdvertisedEndpoint,
		Migration:                 migrationCap{Host: n.MigrationHost, PortRangeStart: n.MigrationPortRangeStart, PortRangeEnd: n.MigrationPortRangeEnd},
		Status:                    string(n.Status),
		CPUCoresTotal:             n.CPUCoresTotal,
		CPUCoresAvailable:         n.CPUCoresAvailable,
		CPUCoresEffective:         n.CPUCoresEffective,
		CPUModel:                  n.CPUModel,
		CPUFlags:                  n.CpuFlags,
		MemoryTotalMiB:            n.MemoryTotalMib,
		MemoryAvailableMiB:        n.MemoryAvailableMib,
		MemoryEffectiveMiB:        n.MemoryEffectiveMib,
		Hugepages2MiB:             n.Hugepages2mibTotal,
		Hugepages1GiB:             n.Hugepages1gibTotal,
		KernelVersion:             n.KernelVersion,
		QEMUVersion:               n.QEMUVersion,
		NumaTopology:              rawJSONOrNull(n.NumaTopology),
		Capabilities:              rawJSONOrEmpty(n.Capabilities),
		AgentVersion:              n.AgentVersion,
		Labels:                    decodeLabels(n.Labels),
		CreatedAt:                 n.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:                 n.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if n.CordonedAt != nil {
		s := n.CordonedAt.UTC().Format(time.RFC3339Nano)
		v.CordonedAt = &s
	}
	if n.LastHeartbeatAt != nil {
		s := n.LastHeartbeatAt.UTC().Format(time.RFC3339Nano)
		v.LastHeartbeatAt = &s
	}
	v.MemoryPressure = toPressureView(n.MemoryPressureSince, n.MemoryPressureCount)
	v.SystemDiskTotalBytes = n.SystemDiskTotalBytes
	v.SystemDiskAvailableBytes = n.SystemDiskAvailableBytes
	v.SystemDiskPressure = toPressureView(n.SystemDiskPressureSince, n.SystemDiskPressureCount)
	if v.CPUFlags == nil {
		v.CPUFlags = []string{}
	}
	// network_conditions is a GET-by-id detail. The list path leaves it
	// empty (never populated) to avoid a per-row fan-out over the status
	// keyspace; GET /v1/nodes/{id} replaces it via networkConditions.
	v.NetworkConditions = []networkConditionView{}
	return v
}

// toPressureView projects the (since, count) pair on a view-backed
// row into the nullable wire shape used by both
// MemoryPressureCondition and SystemDiskPressureCondition. Returns nil
// when the condition is not active — `since == nil`. Production
// invariant: the count is non-zero only while either the condition is
// active OR the debounce is mid-flight; once the count hits zero on a
// cleared pressure both fields are NULL.
func toPressureView(since *time.Time, count int32) *pressureView {
	if since == nil {
		return nil
	}
	return &pressureView{
		Since:            since.UTC().Format(time.RFC3339Nano),
		ConsecutiveCount: count,
	}
}

// networkConditions resolves the per-(node, network) materialisation
// records owned by nodeID into the full-view projection, sorted by network
// name for operator readability. Each record's network id is resolved to a
// name via NetworkByID; a record whose network has been deleted (or never
// existed) is silently skipped rather than erroring - a stale status row
// must not 500 the node read. Returns an empty (non-nil) slice when the
// node has no records so the wire field is `[]`, never null.
func networkConditions(ctx context.Context, s Store, nodeID uuid.UUID) ([]networkConditionView, error) {
	rows, err := s.ListNetworkNodeStatusByNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	out := make([]networkConditionView, 0, len(rows))
	for _, st := range rows {
		net, err := s.NetworkByID(ctx, st.NetworkID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		c := networkConditionView{
			NetworkID:            st.NetworkID.String(),
			Name:                 net.Name,
			ReconciliationStatus: st.ReconciliationStatus,
			ReconciliationError:  st.ReconciliationError,
		}
		if st.LastReconciledAt != nil {
			ts := st.LastReconciledAt.UTC().Format(time.RFC3339Nano)
			c.LastReconciledAt = &ts
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// wgStaleWindow bounds how recent the reporting node's heartbeat must be for
// its established_peers to be trusted; past it, the fabric view reports the
// mesh as not-established (the agent may be dead, its last set is stale).
const wgStaleWindow = 90 * time.Second

// peerEstablished gates a raw established flag on the reporting node's heartbeat
// freshness: an established_peers set from a node that has stopped heartbeating
// is stale and must not claim a live tunnel.
func peerEstablished(rawEstablished bool, selfLastHeartbeat *time.Time, now time.Time) bool {
	if !rawEstablished {
		return false
	}
	if selfLastHeartbeat == nil {
		return false
	}
	return now.Sub(*selfLastHeartbeat) <= wgStaleWindow
}

// nodeWireguard builds the WG underlay fabric block for a node's full view: the
// node's own agent_wireguard identity plus every other agent as a peer with an
// established flag from this node's EstablishedPeers set. The flag is gated by
// the reporting node's heartbeat freshness (selfLastHeartbeat / now) so a dead
// agent's stale handshake set does not claim a live mesh. Returns nil (block
// omitted) when the node has not reported WG yet. Peer node ids resolve to
// names best-effort; a deleted node leaves NodeName nil so the wire never 500s
// on a stale reference.
func nodeWireguard(ctx context.Context, s Store, nodeID uuid.UUID, selfLastHeartbeat *time.Time, now time.Time) (*wireguardView, error) {
	self, err := s.AgentWireguardByNodeID(ctx, nodeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	all, err := s.ListAgentWireguard(ctx)
	if err != nil {
		return nil, err
	}
	established := make(map[string]struct{}, len(self.EstablishedPeers))
	for _, id := range self.EstablishedPeers {
		established[id] = struct{}{}
	}
	peers := make([]wireguardPeerView, 0, len(all))
	for _, rec := range all {
		if rec.NodeID == nodeID {
			continue
		}
		n, nerr := s.NodeByID(ctx, rec.NodeID)
		if nerr != nil {
			// Skip a peer whose node row is deleted (soft-deleted -> ErrNotFound): a
			// stale WG record must not surface in the fabric view. DeleteNode purges
			// the record at the source; this is the sole prune. An unreachable-but-
			// alive node keeps its entry so it rejoins the mesh cleanly on return.
			if errors.Is(nerr, store.ErrNotFound) {
				continue
			}
			return nil, nerr
		}
		pv := wireguardPeerView{NodeID: rec.NodeID.String(), OverlayIP: rec.OverlayIP.String()}
		_, raw := established[rec.NodeID.String()]
		pv.Established = peerEstablished(raw, selfLastHeartbeat, now)
		name := n.Name
		pv.NodeName = &name
		peers = append(peers, pv)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].NodeID < peers[j].NodeID })
	return &wireguardView{
		OverlayIP:            self.OverlayIP.String(),
		PublicKey:            self.PublicKey,
		ListenPort:           self.ListenPort,
		Endpoint:             self.Endpoint,
		ReconciliationStatus: self.ReconciliationStatus,
		ReconciliationError:  self.ReconciliationError,
		Peers:                peers,
	}, nil
}

// nodeListViews projects a page of view-backed node rows into the wire
// entries for GET /v1/nodes: the full effective view for admin / operator
// (full), the reduced summary otherwise. poolNodes is the pool-owning node-id
// set; a row whose id is present derives the hypervisor role.
func nodeListViews(rows []store.NodeEffectiveAvailability, poolNodes map[uuid.UUID]struct{}, full bool) []any {
	views := make([]any, 0, len(rows))
	for _, n := range rows {
		_, ownsPool := poolNodes[n.ID]
		if full {
			views = append(views, toViewEffective(n, ownsPool))
		} else {
			views = append(views, toSummaryViewEffective(n, ownsPool))
		}
	}
	return views
}

// toSummaryViewEffective is the reduced projection counterpart. The
// summary shape excludes resource fields by design (developer / viewer
// roles see only identity and status), so the only "effective" leak is
// that the row was view-backed — no field-level diff vs toSummaryView.
func toSummaryViewEffective(n store.NodeEffectiveAvailability, ownsPool bool) nodeSummaryView {
	v := nodeSummaryView{
		ID:           n.ID.String(),
		Name:         n.Name,
		Architecture: string(n.Architecture),
		Roles:        store.EffectiveRoles(n.GatewayRole, ownsPool),
		Status:       string(n.Status),
		Labels:       decodeLabels(n.Labels),
		CreatedAt:    n.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if n.CordonedAt != nil {
		s := n.CordonedAt.UTC().Format(time.RFC3339Nano)
		v.CordonedAt = &s
	}
	return v
}

// writeNodeResponseEffective is the view-backed sibling of
// writeNodeResponse. admin / operator get the full effective projection;
// other roles get the summary, identical to the raw-row path (summary
// has no resource fields).
//
// conditions is the per-(node, network) materialisation list, attached to
// the full view only. The summary view never carries it, so leaking the
// per-node network state to developer / viewer is structurally impossible.
// Callers that do not surface conditions (cordon / uncordon / create use
// writeNodeResponse on the raw row) pass nil.
//
// wg is the WG underlay fabric block, likewise full-view only and nil when the
// caller does not surface it (or the node has not reported WG yet).
func writeNodeResponseEffective(w http.ResponseWriter, r *http.Request, status int, n store.NodeEffectiveAvailability, ownsPool bool, conditions []networkConditionView, wg *wireguardView, write func(http.ResponseWriter, *http.Request, int, any)) {
	user := auth.UserFromContext(r.Context())
	if user != nil && (user.Role == auth.RoleAdmin || user.Role == auth.RoleOperator) {
		v := toViewEffective(n, ownsPool)
		if conditions != nil {
			v.NetworkConditions = conditions
		}
		v.WireGuard = wg
		write(w, r, status, v)
		return
	}
	write(w, r, status, toSummaryViewEffective(n, ownsPool))
}

// toView builds the full nodeView for admin / operator. ownsPool feeds the
// derived hypervisor role (see toViewEffective).
func toView(n store.Node, ownsPool bool) nodeView {
	v := nodeView{
		ID:                        n.ID.String(),
		Name:                      n.Name,
		Architecture:              string(n.Architecture),
		Roles:                     store.EffectiveRoles(n.GatewayRole, ownsPool),
		AdvertisedEndpoint:        n.AdvertisedEndpoint,
		IngressAdvertisedEndpoint: n.IngressAdvertisedEndpoint,
		Migration:                 migrationCap{Host: n.MigrationHost, PortRangeStart: n.MigrationPortRangeStart, PortRangeEnd: n.MigrationPortRangeEnd},
		Status:                    string(n.Status),
		CPUCoresTotal:             n.CPUCoresTotal,
		CPUCoresAvailable:         n.CPUCoresAvailable,
		CPUModel:                  n.CPUModel,
		CPUFlags:                  n.CpuFlags,
		MemoryTotalMiB:            n.MemoryTotalMib,
		MemoryAvailableMiB:        n.MemoryAvailableMib,
		Hugepages2MiB:             n.Hugepages2mibTotal,
		Hugepages1GiB:             n.Hugepages1gibTotal,
		KernelVersion:             n.KernelVersion,
		QEMUVersion:               n.QEMUVersion,
		NumaTopology:              rawJSONOrNull(n.NumaTopology),
		Capabilities:              rawJSONOrEmpty(n.Capabilities),
		AgentVersion:              n.AgentVersion,
		Labels:                    decodeLabels(n.Labels),
		CreatedAt:                 n.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:                 n.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if n.CordonedAt != nil {
		s := n.CordonedAt.UTC().Format(time.RFC3339Nano)
		v.CordonedAt = &s
	}
	if n.LastHeartbeatAt != nil {
		s := n.LastHeartbeatAt.UTC().Format(time.RFC3339Nano)
		v.LastHeartbeatAt = &s
	}
	v.MemoryPressure = toPressureView(n.MemoryPressureSince, n.MemoryPressureCount)
	v.SystemDiskTotalBytes = n.SystemDiskTotalBytes
	v.SystemDiskAvailableBytes = n.SystemDiskAvailableBytes
	v.SystemDiskPressure = toPressureView(n.SystemDiskPressureSince, n.SystemDiskPressureCount)
	if v.CPUFlags == nil {
		v.CPUFlags = []string{}
	}
	// network_conditions is a GET-by-id detail surfaced only on the
	// effective view; the raw-row path (create / cordon / uncordon) emits
	// an empty list rather than null for wire consistency.
	v.NetworkConditions = []networkConditionView{}
	return v
}

// toSummaryView builds the reduced nodeSummaryView for developer / viewer.
func toSummaryView(n store.Node, ownsPool bool) nodeSummaryView {
	v := nodeSummaryView{
		ID:           n.ID.String(),
		Name:         n.Name,
		Architecture: string(n.Architecture),
		Roles:        store.EffectiveRoles(n.GatewayRole, ownsPool),
		Status:       string(n.Status),
		Labels:       decodeLabels(n.Labels),
		CreatedAt:    n.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	if n.CordonedAt != nil {
		s := n.CordonedAt.UTC().Format(time.RFC3339Nano)
		v.CordonedAt = &s
	}
	return v
}

// writeNodeResponse picks the response shape per the caller's role:
// admin / operator get the full nodeView, others get nodeSummaryView.
// Returns nil-safe even if user is missing — defensive against router
// misconfiguration.
func writeNodeResponse(w http.ResponseWriter, r *http.Request, status int, n store.Node, ownsPool bool, write func(http.ResponseWriter, *http.Request, int, any)) {
	user := auth.UserFromContext(r.Context())
	if user != nil && (user.Role == auth.RoleAdmin || user.Role == auth.RoleOperator) {
		write(w, r, status, toView(n, ownsPool))
		return
	}
	write(w, r, status, toSummaryView(n, ownsPool))
}

// decodeLabels turns the jsonb bytes from the nodes.labels column into a
// string map. The schema constrains labels to be a flat string→string
// map (per the Node OpenAPI schema), so a typed decode is safe; a
// malformed value coming back from the DB indicates corruption rather
// than user input.
func decodeLabels(raw []byte) map[string]string {
	if len(raw) == 0 {
		return map[string]string{}
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]string{}
	}
	if out == nil {
		return map[string]string{}
	}
	return out
}

// rawJSONOrEmpty returns raw as-is if it looks like JSON, otherwise the
// JSON object literal `{}`. Used for `capabilities` (NOT NULL with `'{}'`
// default) so the response never carries `null` for a non-nullable field.
func rawJSONOrEmpty(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return json.RawMessage(raw)
}

// rawJSONOrNull returns raw as-is if non-empty, otherwise the JSON null
// literal. Used for `numa_topology` which IS nullable.
func rawJSONOrNull(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return json.RawMessage(raw)
}

// writeNodeResolveError maps a resolver.Node error to the standard node
// 404 / UUID-rejection / 500 envelopes. Node identifiers in the path
// are name-only - UUID literals surface as 400 validation_failed.
// Used by every handler in this package that loads a node by path param.
func writeNodeResolveError(w http.ResponseWriter, r *http.Request, err error) {
	if resolver.IsUUIDInName(err) {
		response.WriteUUIDNotAllowedError(w, r, "node", "id")
		return
	}
	if resolver.IsNotFound(err) {
		response.WriteError(w, r, http.StatusNotFound,
			response.CodeNotFound, "node not found", nil)
		return
	}
	response.WriteError(w, r, http.StatusInternalServerError,
		response.CodeInternal, "load node", nil)
}
