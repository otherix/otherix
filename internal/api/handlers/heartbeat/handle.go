// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package heartbeat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/response"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Receive serves POST /v1/nodes/{name}/heartbeat. Pre-conditions on
// entry: the route runs behind agentMTLS, so r.Context carries an
// *auth.Agent; absence of one is a routing bug, not a runtime
// situation.
//
// Path identifier is the cluster-unique node name (name-keyed agent
// identity). The agent does not know
// its own UUID; identity is derived from the cert CN at startup and
// carried inline in the URL. The cert→UUID binding inside agent_certs
// stays UUID-keyed (unchanged) — this handler resolves name→UUID
// once via GetNodeByName, then enforces the binding check against
// agent.NodeID, identical to the prior UUID-only contract.
func (h *Handler) Receive(w http.ResponseWriter, r *http.Request) {
	agent := auth.AgentFromContext(r.Context())
	if agent == nil {
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "agent identity missing from context", nil)
		return
	}

	urlNodeName := chi.URLParam(r, "name")
	if urlNodeName == "" {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, "node name missing from path",
			map[string]any{"path_field": "name"})
		return
	}
	node, err := h.store.NodeByName(r.Context(), urlNodeName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			response.WriteError(w, r, http.StatusNotFound,
				response.CodeNotFound, "node not found", nil)
			return
		}
		h.log.ErrorContext(r.Context(), "node lookup by name failed",
			slog.String("node_name", urlNodeName),
			slog.String("error", err.Error()))
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "node lookup failed", nil)
		return
	}
	if node.ID != agent.NodeID {
		// The cert is bound to a different node — refuse without
		// disclosing whether the requested name maps to a UUID that
		// happens to exist.
		response.WriteError(w, r, http.StatusUnauthorized,
			response.CodeUnauthenticated, "client certificate is not bound to this node",
			map[string]any{"reason": "cert_san_unknown"})
		return
	}

	var body requestBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, fmt.Sprintf("invalid request body: %v", err), nil)
		return
	}
	if errResp := validateRequest(&body); errResp != nil {
		response.WriteError(w, r, http.StatusBadRequest,
			response.CodeValidationFailed, errResp.message,
			map[string]any{"field": errResp.field})
		return
	}

	outcome, err := h.project(r.Context(), agent, &body)
	if err != nil {
		var pe *projectionError
		if errors.As(err, &pe) {
			response.WriteError(w, r, pe.status, pe.code, pe.message, pe.details)
			return
		}
		h.log.ErrorContext(r.Context(), "heartbeat projection failed",
			slog.String("node_id", agent.NodeID.String()),
			slog.String("error", err.Error()))
		response.WriteError(w, r, http.StatusInternalServerError,
			response.CodeInternal, "failed to apply heartbeat", nil)
		return
	}

	h.logPressureTransition(r.Context(), agent.NodeID, &body, outcome)

	response.WriteJSON(w, r, http.StatusOK, responseBody{
		ReceivedAt:             time.Now().UTC().Format(time.RFC3339Nano),
		DeclaredPools:          outcome.declaredPools,
		DeclaredVMs:            outcome.declaredVMs,
		DeclaredNetworks:       outcome.declaredNetworks,
		DeclaredWireGuardPeers: outcome.declaredWireGuardPeers,
		SelfOverlayIP:          outcome.selfOverlayIP,
		DeclaredFDB:            outcome.declaredFDB,
		Otwg0MTU:               outcome.otwg0MTU,
	})
}

// logPressureTransition emits one slog line per pressure-state change
// (set → WARN, clear → INFO). Steady-state and counting outcomes stay
// quiet — operators want signal on transitions, not on every heartbeat.
// Logged after InTx commits so the line never appears for a rolled-back
// projection.
func (h *Handler) logPressureTransition(ctx context.Context, nodeID uuid.UUID, body *requestBody, outcome heartbeatOutcome) {
	switch outcome.memory {
	case pressureTransitionSet:
		h.log.WarnContext(ctx, "memory pressure set",
			slog.String("node_id", nodeID.String()),
			slog.Float64("memory_percent", memoryPercent(body)),
			slog.Int("threshold_percent", h.pressureMemory.ThresholdPercent),
			slog.Int("consecutive_required", h.pressureMemory.ConsecutiveRequired),
		)
	case pressureTransitionCleared:
		h.log.InfoContext(ctx, "memory pressure cleared",
			slog.String("node_id", nodeID.String()),
			slog.Float64("memory_percent", memoryPercent(body)),
		)
	}
	switch outcome.systemDisk {
	case pressureTransitionSet:
		h.log.WarnContext(ctx, "system_disk pressure set",
			slog.String("node_id", nodeID.String()),
			slog.Float64("system_disk_percent", systemDiskPercent(body)),
			slog.Int("threshold_percent", h.pressureSystemDisk.ThresholdPercent),
			slog.Int("consecutive_required", h.pressureSystemDisk.ConsecutiveRequired),
		)
	case pressureTransitionCleared:
		h.log.InfoContext(ctx, "system_disk pressure cleared",
			slog.String("node_id", nodeID.String()),
			slog.Float64("system_disk_percent", systemDiskPercent(body)),
		)
	}
}

// memoryPercent returns the percentage available reported by the
// heartbeat body, or 0 when totals are missing or zero. Used for the
// log line only — pressure decision logic lives in
// computePressureTransition and does not rely on this helper.
func memoryPercent(body *requestBody) float64 {
	total := body.Capabilities.MemoryTotalMib
	if total <= 0 {
		return 0
	}
	return float64(body.Resources.MemoryAvailableMib) * 100.0 / float64(total)
}

// systemDiskPercent returns the percentage available reported for the
// root filesystem. Zero when the agent omitted the metric (statfs
// failed). Used for the log line only.
func systemDiskPercent(body *requestBody) float64 {
	total := body.Resources.SystemDiskTotalBytes
	avail := body.Resources.SystemDiskAvailableBytes
	if total == nil || avail == nil || *total <= 0 {
		return 0
	}
	return float64(*avail) * 100.0 / float64(*total)
}

// heartbeatOutcome bundles state-change signals computed during the
// projection that the receive handler needs to surface after the
// transaction commits (currently pressure transitions for memory and
// system_disk). One field per pressure dimension; pool disk transitions
// live entirely on the scan worker side.
type heartbeatOutcome struct {
	memory                 pressureTransitionKind
	systemDisk             pressureTransitionKind
	declaredPools          []declaredPool
	declaredVMs            []declaredVM
	declaredNetworks       []declaredNetwork
	declaredWireGuardPeers []declaredWireGuardPeer
	declaredFDB            []declaredFDBEntry
	selfOverlayIP          *string
	otwg0MTU               *int32
}

// project runs the full state projection in a single transaction.
// It returns the post-commit heartbeatOutcome (currently only pressure
// state transitions) and a typed *projectionError for HTTP-shaped
// failures (404 / 409); any other error is treated as internal.
func (h *Handler) project(ctx context.Context, agent *auth.Agent, body *requestBody) (heartbeatOutcome, error) {
	var outcome heartbeatOutcome
	err := h.store.RunHeartbeatProjection(ctx, func(hp store.HeartbeatProjection) error {
		node, err := hp.NodeForHeartbeat(ctx, agent.NodeID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return &projectionError{
					status: http.StatusNotFound, code: response.CodeNotFound,
					message: "node not found",
				}
			}
			return fmt.Errorf("get node: %v", err)
		}
		if string(node.Architecture) != body.Architecture {
			return &projectionError{
				status: http.StatusConflict, code: response.CodeConflict,
				message: "reported architecture does not match registered architecture",
				details: map[string]any{
					"reason":     "architecture_mismatch",
					"registered": string(node.Architecture),
					"reported":   body.Architecture,
				},
			}
		}
		if node.Status == store.NodeStatusGone {
			return &projectionError{
				status: http.StatusConflict, code: response.CodeConflict,
				message: "node is in gone status; heartbeats rejected",
				details: map[string]any{"reason": "node_gone"},
			}
		}

		if err := h.applyNodeUpdate(ctx, hp, agent.NodeID, body); err != nil {
			return err
		}
		now := time.Now().UTC()
		memKind, err := h.applyMemoryPressure(ctx, hp, agent.NodeID, node, body, now)
		if err != nil {
			return err
		}
		outcome.memory = memKind
		sysKind, err := h.applySystemDiskPressure(ctx, hp, agent.NodeID, node, body, now)
		if err != nil {
			return err
		}
		outcome.systemDisk = sysKind
		if err := h.applyFirmwares(ctx, hp, agent.NodeID, body.Capabilities.Firmwares); err != nil {
			return err
		}
		if err := h.applyVMs(ctx, hp, agent.NodeID, body.VMs); err != nil {
			return err
		}
		if err := h.applyPoolReports(ctx, hp, agent.NodeID, body.Pools); err != nil {
			return err
		}
		if err := h.applyNetworkReports(ctx, hp, agent.NodeID, body.Networks); err != nil {
			return err
		}
		if err := h.applyWireguardReport(ctx, hp, agent.NodeID, body.WireGuard); err != nil {
			return err
		}
		return h.loadDeclared(ctx, hp, agent.NodeID, &outcome)
	})
	return outcome, err
}

// loadDeclared fetches every down-channel desired-state inventory (pools, VMs,
// networks, WG peers) into outcome after the apply phase. Split out of project
// to keep that function's branching under the gocyclo ceiling.
func (h *Handler) loadDeclared(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, outcome *heartbeatOutcome) error {
	// The otwg0 link MTU is independent of whether this agent has a WG record
	// yet: the agent must bring otwg0 up at the right size before its first WG
	// report, so compute it unconditionally from the underlay MTU.
	underlay, err := hp.UnderlayMTU(ctx)
	if err != nil {
		return fmt.Errorf("load underlay mtu: %v", err)
	}
	otwgMTU := underlay - store.WGEncapOverhead
	outcome.otwg0MTU = &otwgMTU
	declaredPools, err := h.loadDeclaredPools(ctx, hp, nodeID)
	if err != nil {
		return err
	}
	outcome.declaredPools = declaredPools
	declaredVMs, err := h.loadDeclaredVMs(ctx, hp, nodeID)
	if err != nil {
		return err
	}
	outcome.declaredVMs = declaredVMs
	declaredNetworks, err := h.loadDeclaredNetworks(ctx, hp)
	if err != nil {
		return err
	}
	outcome.declaredNetworks = declaredNetworks
	declaredWG, err := h.loadDeclaredWireGuardPeers(ctx, hp, nodeID)
	if err != nil {
		return err
	}
	outcome.declaredWireGuardPeers = declaredWG
	declaredFDB, err := h.loadDeclaredFDB(ctx, hp, nodeID)
	if err != nil {
		return err
	}
	outcome.declaredFDB = declaredFDB
	self, err := hp.AgentWireguardByNodeID(ctx, nodeID)
	switch {
	case err == nil:
		supernet, serr := hp.OverlaySupernet(ctx)
		if serr != nil {
			return fmt.Errorf("load overlay supernet: %v", serr)
		}
		cidr := netip.PrefixFrom(self.OverlayIP, supernet.Bits()).String()
		outcome.selfOverlayIP = &cidr
	case errors.Is(err, store.ErrNotFound):
		// No WG report yet for this agent -> leave self_overlay_ip nil.
	default:
		return fmt.Errorf("load self agent_wireguard: %v", err)
	}
	return nil
}

// loadDeclaredVMs returns the per-node VM desired-state inventory the CP
// wants the agent's VM reconciler to converge on. Surfaces as
// `HeartbeatResponse.declared_vms`. The
// underlying SQL orders rows lower(name) asc so the agent's diff stays
// deterministic. Soft-deleted VMs are filtered out at the SQL layer;
// VMs whose vm_runtime.phase has reached 'gone' are excluded too (the
// runtime declared the VM unmaterialised on this node).
func (h *Handler) loadDeclaredVMs(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID) ([]declaredVM, error) {
	rows, err := hp.ListVMsForNodeDeclared(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list vms for node declared: %v", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]declaredVM, 0, len(rows))
	for _, row := range rows {
		out = append(out, declaredVM{
			Name:         row.Name,
			DesiredPhase: string(row.DesiredPhase),
			Generation:   row.Generation,
		})
	}
	return out, nil
}

// applyPoolReports walks each agent-reported pool and applies its
// reconciliation_status / reconciliation_error onto the matching
// `storage_pools` row. The join key is
// (node_id, lower(name)); rows that no longer exist (operator deleted
// the pool mid-tick) yield zero rows affected — the agent reconciles
// on its next tick. failure here propagates as a projection error;
// the transaction rolls back.
func (h *Handler) applyPoolReports(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, reports []poolReport) error {
	for _, p := range reports {
		params := store.UpdateStoragePoolReconciliationParams{
			ReconciliationStatus: p.ReconciliationStatus,
			ReconciliationError:  p.ReconciliationError,
			NodeID:               nodeID,
			Name:                 p.Name,
		}
		if err := hp.UpdateStoragePoolReconciliation(ctx, params); err != nil {
			return fmt.Errorf("update pool reconciliation: %v", err)
		}
	}
	return nil
}

// applyNetworkReports walks each agent-reported network and upserts its
// reconciliation_status / reconciliation_error onto the per-(network, node)
// status record. Unlike pools, networks are cluster-wide and the report keys
// on the network id (uuid). A report whose id does not parse as a uuid is
// skipped with a WARN log and does not fail the heartbeat — a node reporting a
// stale or garbage network id must not 500 the whole projection (mirrors the
// tolerant unknown-pool / unknown-vm paths). A genuine store failure
// propagates as a projection error and rolls the transaction back.
func (h *Handler) applyNetworkReports(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, reports []networkReport) error {
	for _, r := range reports {
		networkID, err := uuid.Parse(r.ID)
		if err != nil {
			h.log.WarnContext(ctx, "heartbeat network id not a uuid; skipping",
				slog.String("node_id", nodeID.String()),
				slog.String("network_id", r.ID))
			continue
		}
		if !validReconciliationStatus(r.ReconciliationStatus) {
			h.log.WarnContext(ctx, "heartbeat network reconciliation_status not in enum; skipping",
				slog.String("node_id", nodeID.String()),
				slog.String("network_id", r.ID),
				slog.String("reconciliation_status", r.ReconciliationStatus))
			continue
		}
		params := store.UpsertNetworkNodeStatusParams{
			NetworkID:            networkID,
			NodeID:               nodeID,
			ReconciliationStatus: r.ReconciliationStatus,
			ReconciliationError:  r.ReconciliationError,
		}
		if err := hp.UpsertNetworkNodeStatus(ctx, params); err != nil {
			return fmt.Errorf("upsert network_node_status: %v", err)
		}
	}
	return nil
}

// validReconciliationStatus reports whether s is one of the
// NetworkNodeStatus.reconciliation_status enum values (pending | ready |
// failed). The CP serves this value back through the public NetworkNodeStatus
// schema, so an out-of-enum value reported by a buggy/old agent must be
// rejected at ingest rather than persisted.
func validReconciliationStatus(s string) bool {
	switch s {
	case "pending", "ready", "failed":
		return true
	default:
		return false
	}
}

// loadDeclaredPools returns the per-node pool inventory the CP wants
// the agent to materialise. Surfaces as `HeartbeatResponse.declared_pools`.
// The query is row-ordered (lower(name) asc) so the
// agent's diff against observed state stays deterministic across
// heartbeats.
func (h *Handler) loadDeclaredPools(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID) ([]declaredPool, error) {
	rows, err := hp.ListStoragePoolsByNode(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list storage pools by node: %v", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]declaredPool, 0, len(rows))
	for _, row := range rows {
		dp := declaredPool{
			Name: row.Name,
			Type: row.Type,
			Path: row.Path,
		}
		if len(row.Config) > 0 {
			cfg := make(map[string]any)
			if err := json.Unmarshal(row.Config, &cfg); err != nil {
				// Storage_pools.config is a JSON object literal per the
				// CHECK constraint, but a malformed value should not
				// take down the whole heartbeat. Log + skip the field.
				h.log.WarnContext(ctx, "storage_pools.config unmarshal failed; emitting empty object",
					slog.String("pool_name", row.Name),
					slog.String("error", err.Error()))
				dp.Config = map[string]any{}
			} else {
				dp.Config = cfg
			}
		}
		out = append(out, dp)
	}
	return out, nil
}

// loadDeclaredNetworks returns the cluster-wide network inventory the CP
// wants every node to materialise. Surfaces as
// `HeartbeatResponse.declared_networks`. Unlike pools and VMs, networks
// are NOT node-scoped: ListNetworks returns every non-deleted network and
// the same set is handed to every node. Sorted by id so the agent's diff
// against observed bridges stays deterministic across heartbeats.
func (h *Handler) loadDeclaredNetworks(ctx context.Context, hp store.HeartbeatProjection) ([]declaredNetwork, error) {
	rows, err := hp.ListNetworks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list networks: %v", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]declaredNetwork, 0, len(rows))
	for _, row := range rows {
		out = append(out, networkToDeclared(row))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// networkToDeclared projects a store.Network onto the heartbeat wire
// shape, rendering the netip Subnet / Gateway pointers to *string
// (canonical CIDR / IP) and leaving them nil when unset.
func networkToDeclared(n store.Network) declaredNetwork {
	dn := declaredNetwork{
		ID:         n.ID.String(),
		Name:       n.Name,
		Type:       string(n.Type),
		Managed:    n.Managed,
		Egress:     string(n.Egress),
		BridgeName: n.BridgeName,
		Mtu:        n.Mtu,
		VNI:        n.VNI,
	}
	if n.Subnet != nil {
		s := n.Subnet.String()
		dn.Subnet = &s
	}
	if n.Gateway != nil {
		g := n.Gateway.String()
		dn.Gateway = &g
	}
	return dn
}

// applyWireguardReport ingests the agent's observed WG state onto its
// agent_wireguard record (allocating its overlay identity on first report). A
// nil report or an empty public_key is skipped. The two store-level WG faults
// are handled differently, by design:
//
//   - A cross-node pubkey collision (two nodes claiming one key) is a genuine
//     conflict, possibly a security signal, that the node cannot resolve and an
//     operator must see. It stays fail-hard: a 409 projection error rolls the
//     whole heartbeat back so the duplicate never silently sticks.
//   - An exhausted overlay supernet is a cluster-capacity condition the node
//     cannot fix on its own. Failing the heartbeat for it would wedge the node:
//     every tick would 409, no observed state (VM runtime, pools, networks,
//     pressure, last_heartbeat_at) would land, and the node would look dead to
//     the scheduler though qemu is healthy. So it is non-fatal: log a WARN, skip
//     the WG upsert for this node this tick, and let the rest of the projection
//     commit. The agent retries on its next heartbeat; once the operator grows
//     the supernet, the allocation succeeds.
//
// Any other store failure wraps to an internal error.
func (h *Handler) applyWireguardReport(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, rep *wireGuardReport) error {
	if rep == nil {
		return nil
	}
	if rep.PublicKey == "" {
		h.log.WarnContext(ctx, "heartbeat wireguard report missing public_key; skipping",
			slog.String("node_id", nodeID.String()))
		return nil
	}
	err := hp.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{
		NodeID:               nodeID,
		PublicKey:            rep.PublicKey,
		Endpoint:             rep.Endpoint,
		ListenPort:           rep.ListenPort,
		EstablishedPeers:     rep.EstablishedPeers,
		ReconciliationStatus: rep.ReconciliationStatus,
		ReconciliationError:  rep.ReconciliationError,
	})
	if err != nil {
		if errors.Is(err, store.ErrAgentWireguardPubkeyInUse) {
			return &projectionError{
				status: http.StatusConflict, code: response.CodeConflict,
				message: "wireguard public key already in use by another node",
				details: map[string]any{"reason": "wireguard_pubkey_in_use"},
			}
		}
		if errors.Is(err, store.ErrOverlaySupernetExhausted) {
			h.log.WarnContext(ctx, "overlay supernet exhausted; skipping wireguard for node this heartbeat",
				slog.String("node_id", nodeID.String()))
			return nil
		}
		return fmt.Errorf("upsert agent_wireguard: %v", err)
	}
	return nil
}

// loadDeclaredWireGuardPeers returns every OTHER agent's WG fabric identity for
// this node's mesh, sorted by node_id. allowed_ips is the peer's overlay /32.
func (h *Handler) loadDeclaredWireGuardPeers(ctx context.Context, hp store.HeartbeatProjection, selfNodeID uuid.UUID) ([]declaredWireGuardPeer, error) {
	recs, err := hp.ListAgentWireguard(ctx)
	if err != nil {
		return nil, fmt.Errorf("list agent_wireguard: %v", err)
	}
	out := make([]declaredWireGuardPeer, 0, len(recs))
	for _, r := range recs {
		if r.NodeID == selfNodeID {
			continue
		}
		// Defense-in-depth: skip a peer whose node row is gone (soft-deleted) or
		// in the terminal "gone" status, so a stale WG record never bleeds into a
		// live agent's mesh. DeleteNode purges the record at the source; this is
		// the belt to that braces.
		node, err := hp.NodeByID(ctx, r.NodeID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("load node %s for wireguard peer: %v", r.NodeID, err)
		}
		if node.Status == store.NodeStatusGone {
			continue
		}
		peerNet := netip.PrefixFrom(r.OverlayIP, 32) // single VTEP host route
		out = append(out, declaredWireGuardPeer{
			NodeID:     r.NodeID.String(),
			PublicKey:  r.PublicKey,
			Endpoint:   r.Endpoint,
			OverlayIP:  r.OverlayIP.String(),
			AllowedIPs: []string{peerNet.String()},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// loadDeclaredFDB computes the controller-authoritative VXLAN FDB this node must
// program, scoped to overlays it has a local VM on. For each such overlay it
// emits a per-remote-VM unicast entry (mac -> remote node's VTEP IP) and one
// all-zeros BUM/flood entry per distinct remote VTEP (head-end replication).
// Remote = a VM on another node; the node never needs FDB for its own local VMs
// (their MACs live on the otb<vni> bridge). Sorted (vni, mac, vtep) for stable
// heartbeats.
func (h *Handler) loadDeclaredFDB(ctx context.Context, hp store.HeartbeatProjection, selfNodeID uuid.UUID) ([]declaredFDBEntry, error) {
	// The placement scan is the FIRST read of the join; it establishes the MVCC
	// revision every other read in this projection pins to (the WG list and the
	// per-node gone-resolver). Pinning the whole join to one snapshot stops a
	// concurrent VM create/delete/migrate from yielding a torn declared_fdb that
	// the agent's authoritative reconciler would prune live entries against.
	placements, rev, err := hp.ListOverlayNICPlacementsPinned(ctx)
	if err != nil {
		return nil, fmt.Errorf("list overlay nic placements: %v", err)
	}
	recs, err := hp.ListAgentWireguardAtRev(ctx, rev)
	if err != nil {
		return nil, fmt.Errorf("list agent_wireguard: %v", err)
	}
	vtepIP := make(map[uuid.UUID]string, len(recs)) // node -> overlay IP
	for _, r := range recs {
		vtepIP[r.NodeID] = r.OverlayIP.String()
	}
	// Overlays this node participates in locally.
	localVNI := make(map[int32]struct{})
	for _, p := range placements {
		if p.NodeID == selfNodeID {
			localVNI[p.VNI] = struct{}{}
		}
	}
	out, skippedNoIP, err := h.buildRemoteFDBEntries(ctx, hp, rev, placements, localVNI, vtepIP, selfNodeID)
	if err != nil {
		return nil, err
	}
	if skippedNoIP > 0 {
		h.log.WarnContext(ctx, "declared_fdb incomplete: owning nodes have no overlay IP yet",
			slog.Int("skipped_placements", skippedNoIP), slog.String("node_id", selfNodeID.String()))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].VNI != out[j].VNI {
			return out[i].VNI < out[j].VNI
		}
		if out[i].MAC != out[j].MAC {
			return out[i].MAC < out[j].MAC
		}
		return out[i].VtepIP < out[j].VtepIP
	})
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// buildRemoteFDBEntries walks placements and emits, for every overlay this node
// participates in locally, a per-remote-VM unicast entry plus one all-zeros
// BUM/flood entry per distinct remote VTEP. It returns the (unsorted) entries
// and the count of placements skipped because the owning node has no overlay IP
// yet (surfaced as one aggregated WARN by the caller). Gone/soft-deleted owning
// nodes are pruned via the memoised liveness guard, symmetric with
// loadDeclaredWireGuardPeers: the reaper advances a long-unreachable node to
// 'gone' WITHOUT orphaning its vm_runtime, so its placements still surface here
// and must not keep an entry pointing at a dead VTEP.
func (h *Handler) buildRemoteFDBEntries(ctx context.Context, hp store.HeartbeatProjection, rev int64, placements []store.OverlayNICPlacement, localVNI map[int32]struct{}, vtepIP map[uuid.UUID]string, selfNodeID uuid.UUID) ([]declaredFDBEntry, int, error) {
	isGone := newGoneResolver(hp, rev)
	var out []declaredFDBEntry
	flood := make(map[string]struct{}) // dedup key "vni|vtep"
	skippedNoIP := 0
	for _, p := range placements {
		if p.NodeID == selfNodeID {
			continue
		}
		if _, ok := localVNI[p.VNI]; !ok {
			continue
		}
		gone, err := isGone(ctx, p.NodeID)
		if err != nil {
			return nil, 0, err
		}
		if gone {
			continue
		}
		ip, ok := vtepIP[p.NodeID]
		if !ok {
			skippedNoIP++
			continue // owning node has no overlay IP yet
		}
		out = append(out, declaredFDBEntry{VNI: p.VNI, MAC: p.Mac.String(), VtepIP: ip})
		fkey := fmt.Sprintf("%d|%s", p.VNI, ip)
		if _, seen := flood[fkey]; !seen {
			flood[fkey] = struct{}{}
			out = append(out, declaredFDBEntry{VNI: p.VNI, MAC: "00:00:00:00:00:00", VtepIP: ip})
		}
	}
	return out, skippedNoIP, nil
}

// newGoneResolver returns a memoised predicate reporting whether a remote node
// is no longer a valid overlay peer: either its row is gone (soft-deleted ->
// ErrNotFound) or it has reached the terminal "gone" status. Results are cached
// per node id so a placement list with many NICs on one node hits the store
// once. Mirrors the gone-skip in loadDeclaredWireGuardPeers.
func newGoneResolver(hp store.HeartbeatProjection, rev int64) func(context.Context, uuid.UUID) (bool, error) {
	cache := make(map[uuid.UUID]bool)
	return func(ctx context.Context, id uuid.UUID) (bool, error) {
		if v, ok := cache[id]; ok {
			return v, nil
		}
		node, err := hp.NodeByIDAtRev(ctx, id, rev)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				cache[id] = true
				return true, nil
			}
			return false, fmt.Errorf("load node %s for fdb: %v", id, err)
		}
		g := node.Status == store.NodeStatusGone
		cache[id] = g
		return g, nil
	}
}

// applyMemoryPressure computes the next memory-pressure state via the
// pure computePressureTransition function and persists it via
// UpdateNodeMemoryPressure. Runs inside the same transaction as the
// rest of the projection so pressure state never drifts ahead of the
// raw metrics that determined it. Returns the transition kind so the
// caller can log set / clear lines after InTx commits.
func (h *Handler) applyMemoryPressure(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, node store.GetNodeForHeartbeatRow, body *requestBody, now time.Time) (pressureTransitionKind, error) {
	availMib := body.Resources.MemoryAvailableMib
	totalMib := body.Capabilities.MemoryTotalMib
	newSince, newCount, kind := computePressureTransition(
		node.MemoryPressureSince,
		node.MemoryPressureCount,
		&availMib,
		&totalMib,
		h.pressureMemory,
		now,
	)
	if newSince == node.MemoryPressureSince && newCount == node.MemoryPressureCount {
		// No-op write avoidance: pure function returned the same state.
		// Common steady-state path; skipping the UPDATE keeps replication
		// and triggers quiet during stable heartbeats.
		return kind, nil
	}
	if err := hp.UpdateNodeMemoryPressure(ctx, store.UpdateNodeMemoryPressureParams{
		ID:                  nodeID,
		MemoryPressureSince: newSince,
		MemoryPressureCount: newCount,
	}); err != nil {
		return pressureTransitionNone, fmt.Errorf("update memory pressure: %v", err)
	}
	return kind, nil
}

// applySystemDiskPressure mirrors applyMemoryPressure for the root
// filesystem dimension. Both pressures share the same debouncing
// pattern; only the raw metrics (bytes vs MiB) and the configured knobs
// differ. Same no-op write avoidance applies — a heartbeat that does
// not change the pressure state writes nothing.
func (h *Handler) applySystemDiskPressure(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, node store.GetNodeForHeartbeatRow, body *requestBody, now time.Time) (pressureTransitionKind, error) {
	newSince, newCount, kind := computePressureTransition(
		node.SystemDiskPressureSince,
		node.SystemDiskPressureCount,
		body.Resources.SystemDiskAvailableBytes,
		body.Resources.SystemDiskTotalBytes,
		h.pressureSystemDisk,
		now,
	)
	if newSince == node.SystemDiskPressureSince && newCount == node.SystemDiskPressureCount {
		return kind, nil
	}
	if err := hp.UpdateNodeSystemDiskPressure(ctx, store.UpdateNodeSystemDiskPressureParams{
		ID:                      nodeID,
		SystemDiskPressureSince: newSince,
		SystemDiskPressureCount: newCount,
	}); err != nil {
		return pressureTransitionNone, fmt.Errorf("update system_disk pressure: %v", err)
	}
	return kind, nil
}

func (h *Handler) applyNodeUpdate(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, body *requestBody) error {
	caps := body.Capabilities
	capsBlob, err := buildCapabilitiesJSON(caps)
	if err != nil {
		return fmt.Errorf("build capabilities jsonb: %v", err)
	}
	numaBlob, err := buildNumaJSON(caps.NumaTopology)
	if err != nil {
		return fmt.Errorf("build numa jsonb: %v", err)
	}

	migHost, migStart, migEnd, err := resolveMigration(ctx, hp, nodeID, body.Migration)
	if err != nil {
		return err
	}

	params := store.UpdateNodeHeartbeatParams{
		ID:                       nodeID,
		AgentVersion:             ptrString(body.AgentVersion),
		MigrationHost:            migHost,
		MigrationPortRangeStart:  migStart,
		MigrationPortRangeEnd:    migEnd,
		CPUCoresTotal:            ptrInt32(caps.CPUCoresTotal),
		CPUCoresAvailable:        ptrInt32(body.Resources.CPUCoresAvailable),
		CPUModel:                 ptrString(caps.CPUModel),
		CpuFlags:                 nonNilStrings(caps.CPUFlags),
		MemoryTotalMib:           ptrInt64(caps.MemoryTotalMib),
		MemoryAvailableMib:       ptrInt64(body.Resources.MemoryAvailableMib),
		Hugepages2mibTotal:       caps.Hugepages2MibTotal,
		Hugepages1gibTotal:       caps.Hugepages1GibTotal,
		KernelVersion:            ptrString(caps.KernelVersion),
		QEMUVersion:              ptrString(caps.QEMUVersion),
		NumaTopology:             numaBlob,
		Capabilities:             capsBlob,
		SystemDiskTotalBytes:     body.Resources.SystemDiskTotalBytes,
		SystemDiskAvailableBytes: body.Resources.SystemDiskAvailableBytes,
	}
	if err := hp.UpdateNodeHeartbeat(ctx, params); err != nil {
		return fmt.Errorf("update node heartbeat: %v", err)
	}
	return nil
}

// resolveMigration returns the migration triple to write back. When
// the heartbeat carries a Migration block, the agent is asserting an
// updated capability — use it. Otherwise reuse the values currently
// stored on the node so UpdateNodeHeartbeat (which always rewrites
// the columns) is a no-op for that triple.
func resolveMigration(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, mig *migrationCapability) (string, int32, int32, error) {
	if mig != nil {
		return mig.Host, mig.PortRangeStart, mig.PortRangeEnd, nil
	}
	row, err := hp.NodeByID(ctx, nodeID)
	if err != nil {
		return "", 0, 0, fmt.Errorf("reload node migration: %v", err)
	}
	return row.MigrationHost, row.MigrationPortRangeStart, row.MigrationPortRangeEnd, nil
}

func (h *Handler) applyFirmwares(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, reports []firmwareReport) error {
	for _, fr := range reports {
		fwID, err := hp.LookupFirmwareByCatalog(ctx, store.LookupFirmwareByCatalogParams{
			Name:         fr.Name,
			Architecture: store.CPUArch(fr.Architecture),
			Type:         store.FirmwareType(fr.Type),
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				h.log.WarnContext(ctx, "heartbeat firmware not in catalogue; skipping",
					slog.String("node_id", nodeID.String()),
					slog.String("firmware_name", fr.Name),
					slog.String("firmware_architecture", fr.Architecture),
					slog.String("firmware_type", fr.Type))
				continue
			}
			return fmt.Errorf("firmware lookup: %v", err)
		}
		params := store.UpsertNodeFirmwareParams{
			NodeID:     nodeID,
			FirmwareID: fwID,
			CodePath:   fr.CodePath,
			VarsPath:   fr.VarsTemplatePath,
			Available:  true,
		}
		if err := hp.UpsertNodeFirmware(ctx, params); err != nil {
			return fmt.Errorf("upsert node_firmware: %v", err)
		}
	}
	return nil
}

func (h *Handler) applyVMs(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, reports []vmReport) error {
	if len(reports) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(reports))
	for _, r := range reports {
		ids = append(ids, r.VMUUID)
	}
	known, err := hp.FilterExistingVMIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("filter vm ids: %v", err)
	}
	knownSet := make(map[uuid.UUID]struct{}, len(known))
	for _, id := range known {
		knownSet[id] = struct{}{}
	}

	for _, r := range reports {
		if _, ok := knownSet[r.VMUUID]; !ok {
			h.log.WarnContext(ctx, "heartbeat references unknown vm; skipping",
				slog.String("node_id", nodeID.String()),
				slog.String("vm_uuid", r.VMUUID.String()))
			continue
		}
		var lastStarted *time.Time
		if r.LastStartedAt != nil {
			t, err := time.Parse(time.RFC3339Nano, *r.LastStartedAt)
			if err != nil {
				h.log.WarnContext(ctx, "heartbeat vm last_started_at not RFC3339; skipping field",
					slog.String("node_id", nodeID.String()),
					slog.String("vm_uuid", r.VMUUID.String()),
					slog.String("value", *r.LastStartedAt))
			} else {
				ts := t.UTC()
				lastStarted = &ts
			}
		}
		obsGen := int64(0)
		if r.ObservedGeneration != nil {
			obsGen = *r.ObservedGeneration
		}
		nodeIDCopy := nodeID
		params := store.UpsertVMRuntimeParams{
			VmID:               r.VMUUID,
			CurrentNodeID:      &nodeIDCopy,
			Phase:              store.VMPhase(r.Phase),
			ObservedGeneration: obsGen,
			QEMUPID:            r.QEMUPID,
			LastStartedAt:      lastStarted,
			LastErrorMessage:   r.LastErrorMessage,
		}
		if err := hp.UpsertVMRuntime(ctx, params); err != nil {
			return fmt.Errorf("upsert vm_runtime: %v", err)
		}
	}
	return nil
}

// projectionError carries an HTTP-shaped failure out of project()
// so the handler entry point can render it through response.WriteError
// without re-classifying every nested call site.
type projectionError struct {
	status  int
	code    response.ErrorCode
	message string
	details map[string]any
}

func (e *projectionError) Error() string {
	return fmt.Sprintf("heartbeat: %d %s: %s", e.status, e.code, e.message)
}

func ptrString(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func ptrInt32(i int32) *int32 {
	v := i
	return &v
}

func ptrInt64(i int64) *int64 {
	v := i
	return &v
}

// nonNilStrings normalises a possibly-nil slice into an empty
// non-nil slice. nodes.cpu_flags is NOT NULL DEFAULT '{}', so a nil
// slice would surface as a NOT NULL violation when written through
// the heartbeat update.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
