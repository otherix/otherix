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
	"github.com/otherix/otherix/internal/api/validation"
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
		OverlayReachability:    outcome.overlayReachability,
		SessionCAPublicPEM:     outcome.sessionCAPublicPEM,
		DeclaredHealthChecks:   outcome.declaredHealthChecks,
		DeclaredLoadBalancers:  outcome.declaredLoadBalancers,
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
	overlayReachability    []overlayReachability
	declaredHealthChecks   []declaredHealthCheck
	declaredLoadBalancers  []declaredLoadBalancer
	selfOverlayIP          *string
	otwg0MTU               *int32
	sessionCAPublicPEM     *string
}

// project runs the full state projection. The steps are a SEQUENCE OF
// INDEPENDENT WRITES (RunHeartbeatProjection is not a single etcd transaction),
// so the node capability + last_heartbeat_at write (applyNodeUpdate) is stamped
// LAST, only after every observed/declared step has committed - a partially
// applied heartbeat therefore never refreshes liveness on a node whose state
// never landed. It returns the post-commit heartbeatOutcome (currently only
// pressure state transitions) and a typed *projectionError for HTTP-shaped
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
		if err := h.applyObservedReports(ctx, hp, agent.NodeID, body); err != nil {
			return err
		}
		if err := h.loadDeclared(ctx, hp, agent.NodeID, &outcome); err != nil {
			return err
		}
		// applyNodeUpdate runs LAST. RunHeartbeatProjection is NOT a single etcd
		// transaction (it is a sequence of independent writes), so if the node
		// capability + last_heartbeat_at write ran first (as it used to) any later
		// step that persistently fails - e.g. a WireGuard pubkey collision throwing
		// 409 every tick - would still refresh last_heartbeat_at each time. The
		// reaper would then never mark the node unreachable/gone and the scheduler
		// would keep placing on it, while none of its observed state ever lands.
		// Stamping liveness only after every observed/declared step above committed
		// makes a persistently-failing projection stop looking alive. Nothing above
		// reads the fields this writes (capabilities, migration triple, ingress
		// endpoint), so the order change is behaviour-preserving on success.
		return h.applyNodeUpdate(ctx, hp, agent.NodeID, body)
	})
	if err != nil {
		return outcome, err
	}
	outcome.sessionCAPublicPEM = h.loadSessionCAPublic(ctx, agent.NodeID)
	return outcome, nil
}

// applyObservedReports runs the observed-state ingest chain (VMs, backend
// health, pools, blob inventories, networks, wireguard) after the node-update
// and pressure steps. Split out of project so that function's branching stays
// under the gocyclo ceiling; the order is preserved from the inline sequence.
func (h *Handler) applyObservedReports(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, body *requestBody) error {
	if err := h.applyVMs(ctx, hp, nodeID, body.VMs); err != nil {
		return err
	}
	if err := h.applyHealthChecks(ctx, hp, nodeID, body.HealthChecks); err != nil {
		return err
	}
	if err := h.applyPublishedListeners(ctx, hp, nodeID, body.PublishedListeners); err != nil {
		return err
	}
	if err := h.applyPoolReports(ctx, hp, nodeID, body.Pools); err != nil {
		return err
	}
	if err := h.applyBlobInventory(ctx, hp, nodeID, body); err != nil {
		return err
	}
	if err := h.applyImageBlobInventory(ctx, hp, nodeID, body); err != nil {
		return err
	}
	if err := h.applyNetworkReports(ctx, hp, nodeID, body.Networks); err != nil {
		return err
	}
	return h.applyWireguardReport(ctx, hp, nodeID, body.WireGuard)
}

// loadSessionCAPublic reads the active ingress-session CA public half for
// down-channel distribution to gateways. It is a top-level read outside the
// projection transaction and fails open: a missing CA or a transient read error
// returns nil rather than failing an otherwise-applied heartbeat.
func (h *Handler) loadSessionCAPublic(ctx context.Context, nodeID uuid.UUID) *string {
	ca, err := h.store.ActiveSessionCA(ctx)
	if err == nil {
		pub := string(ca.PublicKeyPEM)
		return &pub
	}
	if !errors.Is(err, store.ErrNotFound) {
		h.log.WarnContext(ctx, "active session CA lookup failed; omitting from heartbeat",
			slog.String("node_id", nodeID.String()),
			slog.String("error", err.Error()))
	}
	return nil
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
	declaredNetworks, err := h.loadDeclaredNetworks(ctx, hp, nodeID)
	if err != nil {
		return err
	}
	outcome.declaredNetworks = declaredNetworks
	declaredWG, err := h.loadDeclaredWireGuardPeers(ctx, hp, nodeID)
	if err != nil {
		return err
	}
	outcome.declaredWireGuardPeers = declaredWG
	declaredFDB, reachability, err := h.loadDeclaredFDB(ctx, hp, nodeID)
	if err != nil {
		return err
	}
	outcome.declaredFDB = declaredFDB
	outcome.overlayReachability = reachability
	declaredHealthChecks, err := h.loadDeclaredHealthChecks(ctx, hp, nodeID)
	if err != nil {
		return err
	}
	outcome.declaredHealthChecks = declaredHealthChecks
	declaredLoadBalancers, err := h.loadDeclaredLoadBalancers(ctx, hp, nodeID)
	if err != nil {
		return err
	}
	outcome.declaredLoadBalancers = declaredLoadBalancers
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

// loadDeclaredHealthChecks returns the active L4 health probes the CP wants this
// node's agent to run against the load-balancer backends currently placed on it
// (ADR 0027). Surfaces as `HeartbeatResponse.declared_health_checks`. The store
// filters to backends whose observed vm_runtime.current_node_id is this node, so
// the probes follow the VM (a live-migration source keeps probing until cutover).
func (h *Handler) loadDeclaredHealthChecks(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID) ([]declaredHealthCheck, error) {
	targets, err := hp.ListLoadBalancerHealthTargetsForNode(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list load balancer health targets: %v", err)
	}
	if len(targets) == 0 {
		return nil, nil
	}
	out := make([]declaredHealthCheck, 0, len(targets))
	for _, t := range targets {
		out = append(out, declaredHealthCheck{
			VMID:               t.VMID,
			VMName:             t.VMName,
			LBID:               t.LBID,
			Port:               t.HealthCheck.Port,
			IntervalSeconds:    t.HealthCheck.IntervalSeconds,
			TimeoutSeconds:     t.HealthCheck.TimeoutSeconds,
			HealthyThreshold:   t.HealthCheck.HealthyThreshold,
			UnhealthyThreshold: t.HealthCheck.UnhealthyThreshold,
		})
	}
	return out, nil
}

// loadDeclaredLoadBalancers returns the published load balancers a gateway-role
// node must bind a public L4 listener for, each with its resolved eligible
// backend set. Surfaces as `HeartbeatResponse.declared_load_balancers`. It is
// gated on the gateway role exactly like gatewayAddrsForNode: a non-gateway node
// returns nil (never receives listener state) without running the published
// backend scan. The set is node-independent (the store resolver takes no nodeID),
// so every gateway node gets the same payload; the agent binds exactly the ports
// in this list and closes the rest.
func (h *Handler) loadDeclaredLoadBalancers(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID) ([]declaredLoadBalancer, error) {
	node, err := hp.NodeByID(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("node by id %s: %v", nodeID, err)
	}
	if !node.HasRole(store.NodeRoleGateway) {
		return nil, nil
	}
	lbs, err := hp.ListPublishedLoadBalancerBackends(ctx)
	if err != nil {
		return nil, fmt.Errorf("list published load balancer backends: %v", err)
	}
	if len(lbs) == 0 {
		return nil, nil
	}
	out := make([]declaredLoadBalancer, 0, len(lbs))
	for _, lb := range lbs {
		backends := make([]declaredBackend, 0, len(lb.Backends))
		for _, b := range lb.Backends {
			backends = append(backends, declaredBackend{
				VMID:      b.VMID,
				OverlayIP: b.OverlayIP.String(),
				MAC:       b.MAC.String(),
				Healthy:   b.Healthy,
			})
		}
		out = append(out, declaredLoadBalancer{
			LBID:          lb.LBID,
			PublishedPort: lb.PublishedPort,
			Protocol:      lb.Protocol,
			BackendPort:   lb.BackendPort,
			SourceCIDRs:   lb.SourceCIDRs,
			Backends:      backends,
		})
	}
	return out, nil
}

// applyPoolReports walks each agent-reported pool and applies its
// reconciliation_status / reconciliation_error onto the matching
// `storage_pools` row, then persists the pool's reported image inventory as
// observed state. Both writes share the (node_id, lower(name)) join key; rows
// that no longer exist (operator deleted the pool mid-tick) are skipped — the
// reconciliation write is a silent no-op store-side, and the inventory write is
// skipped here once the resolver reports no match. A genuine store failure on
// either write propagates as a projection error; the transaction rolls back.
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
		if err := h.applyPoolImageInventory(ctx, hp, nodeID, p); err != nil {
			return err
		}
	}
	return nil
}

// applyPoolImageInventory persists the agent-reported image inventory for one
// pool as observed state, keyed by the pool's UUID resolved via the same
// (node_id, lower(name)) lookup the reconciliation write uses. A pool that no
// longer resolves (deleted mid-tick) skips the inventory write — a write keyed
// on a non-existent pool would write an orphan key, so the non-match is a safe
// no-op, not an error. Each image's imported_at is parsed tolerantly: a malformed
// value degrades to zero time (the image's identity is basename+sha256, not the
// timestamp) and never drops the image or fails the heartbeat, mirroring the
// last_started_at handling in applyVMs.
func (h *Handler) applyPoolImageInventory(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, p poolReport) error {
	poolID, found, err := hp.StoragePoolIDByNodeName(ctx, nodeID, p.Name)
	if err != nil {
		return fmt.Errorf("resolve pool id for inventory: %v", err)
	}
	if !found {
		return nil
	}
	if p.ImagesUnavailable {
		// The agent could not enumerate this pool's images this tick (a transient
		// ListImages error). Preserve the prior inventory rather than clear it: an
		// empty upsert deletes the inventory, and a transient producer error must
		// not wipe observed state (fail-closed).
		return nil
	}
	images := make([]store.PoolImage, 0, len(p.Images))
	for _, img := range p.Images {
		var importedAt time.Time
		if img.ImportedAt != "" {
			t, perr := time.Parse(time.RFC3339Nano, img.ImportedAt)
			if perr != nil {
				h.log.WarnContext(ctx, "heartbeat pool image imported_at not RFC3339; storing zero time",
					slog.String("node_id", nodeID.String()),
					slog.String("pool_name", p.Name),
					slog.String("basename", img.Basename),
					slog.String("value", img.ImportedAt))
			} else {
				importedAt = t.UTC()
			}
		}
		images = append(images, store.PoolImage{
			Basename:         img.Basename,
			ChecksumSha256:   img.SHA256,
			SizeBytes:        img.SizeBytes,
			VirtualSizeBytes: img.VirtualSizeBytes,
			Format:           img.Format,
			ImportedAt:       importedAt,
		})
	}
	if err := hp.UpsertPoolImageInventory(ctx, poolID, images); err != nil {
		return fmt.Errorf("upsert pool image inventory: %v", err)
	}
	return nil
}

// applyBlobInventory persists the agent-reported per-node artifact-store blob
// inventory as observed state (the holder-discovery source for the cross-node
// pull saga). Fail-closed: on blobs_unavailable the agent could not
// enumerate its store this tick, so the CP PRESERVES the prior inventory (skip
// the upsert) rather than clearing it - a transient producer error must not wipe
// the holder-discovery source (mirrors the images_unavailable handling).
// Otherwise the reported list is authoritative (an empty list clears
// the inventory). Unlike pool image inventory, blobs are node-level, not
// pool-scoped, so there is no (node_id, name) resolution step.
// maxBlobSizeBytes is the sane ceiling for a heartbeat-reported blob size (64
// GiB, the same family as the consumer's absolute pull cap). The reported size
// becomes the bound the consumer pull is capped to, so an unvalidated value is a
// control: a huge size raises the cap arbitrarily and a negative one disables it
// (the consumer's > 0 gate). A node is not trusted to set its own pull bound.
const maxBlobSizeBytes = 64 << 30

// clampBlobSize sanitizes a heartbeat-reported blob size before it is stored and
// later used as the consumer's pull cap. A negative or over-ceiling value is
// clamped to 0 ("unknown size"): the entry still counts as a holder for
// durability, but the consumer then falls back to its own absolute cap rather
// than trusting a node-supplied bound. A normal size passes through unchanged.
func (h *Handler) clampBlobSize(ctx context.Context, nodeID uuid.UUID, digest string, size int64) int64 {
	if size < 0 || size > maxBlobSizeBytes {
		h.log.WarnContext(ctx, "clamping out-of-range heartbeat blob size to unknown",
			slog.String("node_id", nodeID.String()),
			slog.String("digest", digest),
			slog.Int64("reported_size_bytes", size))
		return 0
	}
	return size
}

func (h *Handler) applyBlobInventory(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, body *requestBody) error {
	if body.BlobsUnavailable {
		return nil
	}
	blobs := make([]store.NodeBlob, 0, len(body.Blobs))
	for _, b := range body.Blobs {
		// A blob inventory is incremental, so a single malformed digest drops only
		// that entry (keeping the rest of the node's inventory) rather than
		// rejecting the whole report. A non-hex digest would otherwise pollute
		// holder-discovery accounting and become a path component on the holder.
		if err := validation.ValidateBlobDigest(b.Digest); err != nil {
			h.log.WarnContext(ctx, "dropping node blob with invalid digest",
				slog.String("node_id", nodeID.String()), slog.String("digest", b.Digest))
			continue
		}
		blobs = append(blobs, store.NodeBlob{Digest: b.Digest, SizeBytes: h.clampBlobSize(ctx, nodeID, b.Digest, b.SizeBytes)})
	}
	if err := hp.UpsertNodeBlobInventory(ctx, nodeID, blobs); err != nil {
		return fmt.Errorf("upsert node blob inventory: %v", err)
	}
	return nil
}

// applyImageBlobInventory projects the node's reported pinned-image cache
// inventory into the image_blobs family. Invalid digests are dropped (kept
// separate from the rest). The unavailable flag preserves the prior inventory
// rather than clearing it (a transient List error must not look like "the node
// dropped every cached image"). Mirrors applyBlobInventory; the families are
// kept separate so the image tier never feeds the durability reconcile.
func (h *Handler) applyImageBlobInventory(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, body *requestBody) error {
	if body.ImageBlobsUnavailable {
		return nil
	}
	blobs := make([]store.NodeBlob, 0, len(body.ImageBlobs))
	for _, b := range body.ImageBlobs {
		if err := validation.ValidateBlobDigest(b.Digest); err != nil {
			h.log.WarnContext(ctx, "dropping node image blob with invalid digest",
				slog.String("node_id", nodeID.String()), slog.String("digest", b.Digest))
			continue
		}
		blobs = append(blobs, store.NodeBlob{Digest: b.Digest, SizeBytes: h.clampBlobSize(ctx, nodeID, b.Digest, b.SizeBytes)})
	}
	if err := hp.UpsertNodeImageBlobInventory(ctx, nodeID, blobs); err != nil {
		return fmt.Errorf("upsert node image blob inventory: %v", err)
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
			ActiveSessions:       r.ActiveSessions,
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
func (h *Handler) loadDeclaredNetworks(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID) ([]declaredNetwork, error) {
	rows, err := hp.ListNetworks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list networks: %v", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	gwAddrs, err := h.gatewayAddrsForNode(ctx, hp, nodeID)
	if err != nil {
		return nil, err
	}
	out := make([]declaredNetwork, 0, len(rows))
	for _, row := range rows {
		dn := networkToDeclared(row)
		if row.DhcpEnabled {
			nics, nerr := hp.ListVMNicsByNetwork(ctx, row.ID)
			if nerr != nil {
				return nil, fmt.Errorf("list nics for network %s: %v", row.ID, nerr)
			}
			dn.Reservations = reservationsFromNICs(nics)
		}
		if ga, ok := gwAddrs[row.ID]; ok {
			dn.GatewayAddr = ga
		}
		out = append(out, dn)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// gatewayAddrsForNode returns the per-network tenant IP + unicast MAC the
// heartbeating node must own as an ingress gateway, keyed by network id. It is
// non-empty only when the node is a gateway: a gateway covers one or more
// overlay networks (its memberships), and on each it claims a tenant IP + a
// distinct unicast MAC so the host originates at the tenant IP and the bridge
// owns the MAC advertised to peers. A hypervisor node holds no memberships and
// gets an empty map (nil gateway_addr on every network).
func (h *Handler) gatewayAddrsForNode(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID) (map[uuid.UUID]*gatewayAddr, error) {
	node, err := hp.NodeByID(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("node by id %s: %v", nodeID, err)
	}
	if !node.HasRole(store.NodeRoleGateway) {
		return nil, nil
	}
	memberships, err := hp.ListGatewayMembershipsForGateway(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("list gateway memberships for %s: %v", nodeID, err)
	}
	if len(memberships) == 0 {
		return nil, nil
	}
	out := make(map[uuid.UUID]*gatewayAddr, len(memberships))
	for _, m := range memberships {
		out[m.NetworkID] = &gatewayAddr{IP: m.TenantIP.String(), MAC: m.MAC.String()}
	}
	return out, nil
}

// reservationsFromNICs projects NIC rows onto wire reservations, skipping NICs
// without an allocated IPv4. Sorted by MAC so heartbeats are deterministic.
func reservationsFromNICs(nics []store.VMNic) []dhcpReservation {
	out := make([]dhcpReservation, 0, len(nics))
	for _, n := range nics {
		if n.Ipv4Address == nil {
			continue
		}
		out = append(out, dhcpReservation{MAC: n.MacAddress.String(), IP: n.Ipv4Address.String()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MAC < out[j].MAC })
	if len(out) == 0 {
		return nil
	}
	return out
}

// networkToDeclared projects a store.Network onto the heartbeat wire
// shape, rendering the netip Subnet / Gateway pointers to *string
// (canonical CIDR / IP) and leaving them nil when unset.
func networkToDeclared(n store.Network) declaredNetwork {
	dn := declaredNetwork{
		ID:          n.ID.String(),
		Name:        n.Name,
		Type:        string(n.Type),
		Managed:     n.Managed,
		Egress:      string(n.Egress),
		BridgeName:  n.BridgeName,
		Mtu:         n.Mtu,
		VNI:         n.VNI,
		DhcpEnabled: n.DhcpEnabled,
		DNSEnabled:  n.DNSEnabled,
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

// loadDeclaredWireGuardPeers returns this node's declared WireGuard peer set for
// the fabric down-channel. It reads every agent's WG fabric identity and node
// row, builds the routing inputs (Endpoint = the advertised WG endpoint, "" when
// NAT'd; IsGateway from the node's gateway role; the reachable-peer set from
// EstablishedPeers) and delegates the policy to ComputeWireGuardRouting: a
// reachable peer gets a direct entry, a NAT'd<->NAT'd pair is relayed through a
// deterministic gateway. self is looked up in the same pass; a zero-value self
// (first heartbeat, before this node's own WG upsert lands) still yields direct
// entries for reachable peers and simply wires no relays.
func (h *Handler) loadDeclaredWireGuardPeers(ctx context.Context, hp store.HeartbeatProjection, selfNodeID uuid.UUID) ([]declaredWireGuardPeer, error) {
	recs, err := hp.ListAgentWireguard(ctx)
	if err != nil {
		return nil, fmt.Errorf("list agent_wireguard: %v", err)
	}
	var self RoutingNode
	peers := make([]RoutingNode, 0, len(recs))
	for _, r := range recs {
		// Skip a peer whose node row is deleted (soft-deleted -> ErrNotFound), so a
		// stale WG record never bleeds into a live agent's mesh. DeleteNode purges
		// the record at the source; this is the sole prune. An unreachable-but-alive
		// node keeps its peer entry so it rejoins the mesh cleanly on return. The
		// same read supplies the node's gateway role.
		node, err := hp.NodeByID(ctx, r.NodeID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("load node %s for wireguard peer: %v", r.NodeID, err)
		}
		rn := RoutingNode{
			NodeID:           r.NodeID,
			PublicKey:        r.PublicKey,
			OverlayIP:        r.OverlayIP,
			Endpoint:         r.Endpoint,
			IsGateway:        node.GatewayRole,
			EstablishedPeers: establishedPeerSet(r.EstablishedPeers),
		}
		if r.NodeID == selfNodeID {
			self = rn
			continue
		}
		peers = append(peers, rn)
	}
	out := ComputeWireGuardRouting(self, peers)
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// establishedPeerSet converts the persisted node-id strings into the set
// ComputeWireGuardRouting keys reachability on; an unparseable id is dropped.
func establishedPeerSet(ids []string) map[uuid.UUID]bool {
	if len(ids) == 0 {
		return nil
	}
	set := make(map[uuid.UUID]bool, len(ids))
	for _, s := range ids {
		if id, err := uuid.Parse(s); err == nil {
			set[id] = true
		}
	}
	return set
}

// loadDeclaredFDB computes the controller-authoritative VXLAN FDB this node must
// program, scoped to overlays it has a local VM on. For each such overlay it
// emits a per-remote-VM unicast entry (mac -> remote node's VTEP IP) and one
// all-zeros BUM/flood entry per distinct remote VTEP (head-end replication).
// Remote = a VM on another node; the node never needs FDB for its own local VMs
// (their MACs live on the otvb<vni> bridge). Sorted (vni, mac, vtep) for stable
// heartbeats.
func (h *Handler) loadDeclaredFDB(ctx context.Context, hp store.HeartbeatProjection, selfNodeID uuid.UUID) ([]declaredFDBEntry, []overlayReachability, error) {
	// The placement scan is the FIRST read of the join; it establishes the MVCC
	// revision every other read in this projection pins to (the WG list and the
	// per-node deleted-resolver). Pinning the whole join to one snapshot stops a
	// concurrent VM create/delete/migrate from yielding a torn declared_fdb that
	// the agent's authoritative reconciler would prune live entries against.
	placements, rev, err := hp.ListOverlayNICPlacementsPinned(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list overlay nic placements: %v", err)
	}
	recs, err := hp.ListAgentWireguardAtRev(ctx, rev)
	if err != nil {
		return nil, nil, fmt.Errorf("list agent_wireguard: %v", err)
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
	out, skippedPerVNI, floodPerVNI, err := h.buildRemoteFDBEntries(ctx, hp, rev, placements, localVNI, vtepIP, selfNodeID)
	if err != nil {
		return nil, nil, err
	}
	// The self node's agent_wireguard record (already in recs) carries the set of
	// remote node ids it currently has an established WireGuard handshake with.
	// Correlating it with each VNI's distinct flood-target node ids yields the
	// established/total reachability signal. Absent self record -> empty set ->
	// established_peers=0, which is the correct "no live tunnels reported" state.
	established := selfEstablishedSet(recs, selfNodeID)
	reachability := summarizeReachability(skippedPerVNI, floodPerVNI, established)
	if total := totalSkipped(skippedPerVNI); total > 0 {
		h.log.WarnContext(ctx, "declared_fdb incomplete: owning nodes have no overlay IP yet",
			slog.Int("skipped_placements", total), slog.String("node_id", selfNodeID.String()))
	}
	if unest := totalUnestablished(reachability); unest > 0 {
		// Non-blocking: a flood target up in the FDB with no live tunnel still
		// blackholes BUM traffic until the tunnel establishes. Surfaced for
		// operators; never gates the overlay (rekey would flap it otherwise).
		h.log.WarnContext(ctx, "overlay flood targets have no established wireguard tunnel",
			slog.Int("unestablished_flood_targets", unest), slog.String("node_id", selfNodeID.String()))
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
		out = nil
	}
	return out, reachability, nil
}

// summarizeReachability projects the per-VNI skipped-no-IP count and the per-VNI
// flood-target node-id sets into the sorted wire slice the heartbeat surfaces. For
// each VNI total_peers is the number of distinct flood-target VTEPs (remote nodes
// with an overlay IP) and established_peers is how many of those the reporting node
// has a live WireGuard handshake with (from its agent_wireguard.established_peers).
// A VNI is surfaced when it has any flood target OR any skipped placement; a VNI
// with neither carries no signal at all (omitempty drops the field). The signal is
// observability only — it never feeds the overlay ready/pending decision.
func summarizeReachability(skippedPerVNI map[int32]int, floodPerVNI map[int32]map[uuid.UUID]struct{}, established map[string]struct{}) []overlayReachability {
	vnis := make(map[int32]struct{}, len(skippedPerVNI)+len(floodPerVNI))
	for vni := range skippedPerVNI {
		vnis[vni] = struct{}{}
	}
	for vni := range floodPerVNI {
		vnis[vni] = struct{}{}
	}
	if len(vnis) == 0 {
		return nil
	}
	out := make([]overlayReachability, 0, len(vnis))
	for vni := range vnis {
		targets := floodPerVNI[vni]
		var establishedCount int
		for nodeID := range targets {
			if _, ok := established[nodeID.String()]; ok {
				establishedCount++
			}
		}
		out = append(out, overlayReachability{
			VNI:              vni,
			SkippedNoIP:      int32(skippedPerVNI[vni]), //nolint:gosec // per-VNI placement count, bounded by cluster size
			EstablishedPeers: int32(establishedCount),   //nolint:gosec // per-VNI flood-target count, bounded by cluster size
			TotalPeers:       int32(len(targets)),       //nolint:gosec // per-VNI flood-target count, bounded by cluster size
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VNI < out[j].VNI })
	return out
}

// selfEstablishedSet returns the set of remote node-id strings the reporting node
// (selfNodeID) currently has an established WireGuard handshake with, read from its
// own agent_wireguard record in recs. Empty when the self node has no record yet —
// the correct "no live tunnels reported" state, not an error.
func selfEstablishedSet(recs []store.AgentWireguard, selfNodeID uuid.UUID) map[string]struct{} {
	for _, r := range recs {
		if r.NodeID != selfNodeID {
			continue
		}
		set := make(map[string]struct{}, len(r.EstablishedPeers))
		for _, p := range r.EstablishedPeers {
			set[p] = struct{}{}
		}
		return set
	}
	return map[string]struct{}{}
}

// totalSkipped sums the per-VNI skip counts for the single aggregated WARN.
func totalSkipped(skippedPerVNI map[int32]int) int {
	total := 0
	for _, n := range skippedPerVNI {
		total += n
	}
	return total
}

// totalUnestablished sums, across VNIs, the flood targets that are programmed but
// lack an established WireGuard tunnel (total_peers - established_peers) for the
// single aggregated WARN. Observability only; it gates nothing.
func totalUnestablished(reach []overlayReachability) int {
	total := 0
	for _, r := range reach {
		total += int(r.TotalPeers - r.EstablishedPeers)
	}
	return total
}

// buildRemoteFDBEntries walks placements and emits, for every overlay this node
// participates in locally, a per-remote-VM unicast entry plus one all-zeros
// BUM/flood entry per distinct remote VTEP. It returns the (unsorted) entries
// and a per-VNI count of placements skipped because the owning node has no
// overlay IP yet (surfaced as the non-blocking overlay_reachability signal plus
// one aggregated WARN by the caller). Only soft-deleted owning nodes are pruned
// via the memoised deleted-resolver, symmetric with loadDeclaredWireGuardPeers:
// DeleteNode purges the record at the source, so a deleted node's placements must
// not keep an entry pointing at a dead VTEP. An unreachable-but-alive node keeps
// its entry so it rejoins the mesh cleanly on return.
func (h *Handler) buildRemoteFDBEntries(ctx context.Context, hp store.HeartbeatProjection, rev int64, placements []store.OverlayNICPlacement, localVNI map[int32]struct{}, vtepIP map[uuid.UUID]string, selfNodeID uuid.UUID) ([]declaredFDBEntry, map[int32]int, map[int32]map[uuid.UUID]struct{}, error) {
	isDeleted := newDeletedResolver(hp, rev)
	var out []declaredFDBEntry
	flood := make(map[string]struct{}) // dedup key "vni|vtep"
	skippedPerVNI := make(map[int32]int)
	// floodPerVNI records the distinct flood-target node ids per VNI (a node maps
	// 1:1 to its VTEP IP). It is the total_peers denominator the reachability
	// signal correlates against the self node's established-peer set.
	floodPerVNI := make(map[int32]map[uuid.UUID]struct{})
	for _, p := range placements {
		if p.NodeID == selfNodeID {
			continue
		}
		if _, ok := localVNI[p.VNI]; !ok {
			continue
		}
		deleted, err := isDeleted(ctx, p.NodeID)
		if err != nil {
			return nil, nil, nil, err
		}
		if deleted {
			continue
		}
		ip, ok := vtepIP[p.NodeID]
		if !ok {
			skippedPerVNI[p.VNI]++
			continue // owning node has no overlay IP yet
		}
		out = append(out, declaredFDBEntry{VNI: p.VNI, MAC: p.Mac.String(), VtepIP: ip})
		fkey := fmt.Sprintf("%d|%s", p.VNI, ip)
		if _, seen := flood[fkey]; !seen {
			flood[fkey] = struct{}{}
			out = append(out, declaredFDBEntry{VNI: p.VNI, MAC: "00:00:00:00:00:00", VtepIP: ip})
			targets := floodPerVNI[p.VNI]
			if targets == nil {
				targets = make(map[uuid.UUID]struct{})
				floodPerVNI[p.VNI] = targets
			}
			targets[p.NodeID] = struct{}{}
		}
	}
	return out, skippedPerVNI, floodPerVNI, nil
}

// newDeletedResolver returns a memoised predicate reporting whether a remote
// node's row is deleted (soft-deleted -> ErrNotFound), in which case it is no
// longer a valid overlay peer. Results are cached per node id so a placement list
// with many NICs on one node hits the store once. A node that loads successfully
// caches false: an unreachable-but-alive node keeps its FDB entry so it rejoins
// the mesh cleanly on return. Mirrors the ErrNotFound skip in
// loadDeclaredWireGuardPeers; DeleteNode purges the record at the source.
func newDeletedResolver(hp store.HeartbeatProjection, rev int64) func(context.Context, uuid.UUID) (bool, error) {
	cache := make(map[uuid.UUID]bool)
	return func(ctx context.Context, id uuid.UUID) (bool, error) {
		if v, ok := cache[id]; ok {
			return v, nil
		}
		if _, err := hp.NodeByIDAtRev(ctx, id, rev); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				cache[id] = true
				return true, nil
			}
			return false, fmt.Errorf("load node %s for fdb: %v", id, err)
		}
		cache[id] = false
		return false, nil
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
		if errors.Is(err, store.ErrConcurrentUpdate) {
			h.log.WarnContext(ctx, "node row contended; skipping memory pressure write this heartbeat",
				slog.String("node_id", nodeID.String()))
			return pressureTransitionNone, nil
		}
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
		if errors.Is(err, store.ErrConcurrentUpdate) {
			h.log.WarnContext(ctx, "node row contended; skipping system_disk pressure write this heartbeat",
				slog.String("node_id", nodeID.String()))
			return pressureTransitionNone, nil
		}
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
		ID:                        nodeID,
		AgentVersion:              ptrString(body.AgentVersion),
		MigrationHost:             migHost,
		MigrationPortRangeStart:   migStart,
		MigrationPortRangeEnd:     migEnd,
		CPUCoresTotal:             ptrInt32(caps.CPUCoresTotal),
		CPUCoresAvailable:         ptrInt32(body.Resources.CPUCoresAvailable),
		CPUModel:                  ptrString(caps.CPUModel),
		CpuFlags:                  nonNilStrings(caps.CPUFlags),
		MemoryTotalMib:            ptrInt64(caps.MemoryTotalMib),
		MemoryAvailableMib:        ptrInt64(body.Resources.MemoryAvailableMib),
		Hugepages2mibTotal:        caps.Hugepages2MibTotal,
		Hugepages1gibTotal:        caps.Hugepages1GibTotal,
		KernelVersion:             ptrString(caps.KernelVersion),
		QEMUVersion:               ptrString(caps.QEMUVersion),
		NumaTopology:              numaBlob,
		Capabilities:              capsBlob,
		SystemDiskTotalBytes:      body.Resources.SystemDiskTotalBytes,
		SystemDiskAvailableBytes:  body.Resources.SystemDiskAvailableBytes,
		IngressAdvertisedEndpoint: body.IngressAdvertisedEndpoint,
	}
	if err := hp.UpdateNodeHeartbeat(ctx, params); err != nil {
		if errors.Is(err, store.ErrConcurrentUpdate) {
			h.log.WarnContext(ctx, "node row contended; skipping capability write this heartbeat",
				slog.String("node_id", nodeID.String()))
			return nil
		}
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
	pinned, err := hp.FilterVMIDsPinnedToNode(ctx, nodeID, ids)
	if err != nil {
		return fmt.Errorf("filter pinned vm ids: %v", err)
	}
	pinnedSet := make(map[uuid.UUID]struct{}, len(pinned))
	for _, id := range pinned {
		pinnedSet[id] = struct{}{}
	}

	for _, r := range reports {
		if _, ok := knownSet[r.VMUUID]; !ok {
			h.log.WarnContext(ctx, "heartbeat references unknown vm; skipping",
				slog.String("node_id", nodeID.String()),
				slog.String("vm_uuid", r.VMUUID.String()))
			continue
		}
		// Placement-authority gate. current_node_id feeds the
		// overlay FDB projection and force-delete orphaning, so a heartbeat
		// must never move a VM's runtime to a node the scheduler did not pin
		// it to: the cert binding authenticates WHO reports, not WHICH VMs
		// they may claim. Fail-closed: a VM pinned to another node, or not
		// pinned at all (unscheduled), is skipped, never claimed.
		//
		// Placement-authority gate is migration-aware via FilterVMIDsPinnedToNode (admits an active migration's target). See migration design D3.
		if _, ok := pinnedSet[r.VMUUID]; !ok {
			h.log.WarnContext(ctx, "heartbeat claims a vm not pinned to the reporting node; skipping runtime claim",
				slog.String("node_id", nodeID.String()),
				slog.String("vm_uuid", r.VMUUID.String()))
			continue
		}
		if err := h.applyVMReport(ctx, hp, nodeID, r); err != nil {
			return err
		}
	}
	return nil
}

// applyVMReport upserts a single VM runtime row from one heartbeat vmReport. The
// caller has already gated the report through the known/pinned placement checks.
func (h *Handler) applyVMReport(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, r vmReport) error {
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
	// Re-read the vms row fresh and decide the runtime claim against THIS pin and
	// THIS ModRevision, then commit the write under a compare on the rev (inside
	// UpsertVMRuntime). The batch placement gate (FilterVMIDsPinnedToNode)
	// admitted this report, but rev-less and earlier; a delete or a migration
	// cutover can land in the window between that gate and this write. Deciding
	// and CAS-ing off the same fresh read closes both TOCTOUs: a row that moves
	// after this read fails the compare, and a pin that already moved before it is
	// caught here. A benign vms-row writer (a user PATCH, a scheduler bind) landing
	// in this tiny read-to-commit window can also fail the compare and drop this
	// tick's runtime update; that self-heals on the next heartbeat, which re-reads
	// a fresh rev.
	vm, vmRev, err := hp.VMWithRev(ctx, r.VMUUID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Deleted between the gate and here; do not resurrect a runtime row on
			// a tombstoned VM.
			return nil
		}
		return fmt.Errorf("reread vm for runtime claim: %v", err)
	}

	// Claim authority, fail-closed. The heartbeat may write the runtime row only
	// as the VM's current owner: the pinned node, or - during an active migration
	// - the target writing on behalf of the source. The epoch fence keeps
	// current_node_id at the source until CommitMigrationCutover flips the pin, so
	// a source and a target report during a move do not race last-writer-wins and
	// flap the overlay FDB key. Any other case (the pin moved out from under this
	// report - a cutover raced - or an unpinned VM) is skipped, never claimed.
	var claimNode uuid.UUID
	switch {
	case vm.PinnedNodeID != nil && *vm.PinnedNodeID == nodeID:
		claimNode = nodeID
	default:
		mig, active, err := hp.ActiveMigrationForVM(ctx, r.VMUUID)
		if err != nil {
			return fmt.Errorf("active migration lookup: %v", err)
		}
		if !active || mig.TargetNodeID == nil || *mig.TargetNodeID != nodeID || mig.SourceNodeID == nil {
			return nil
		}
		claimNode = *mig.SourceNodeID
	}

	nodeIDCopy := claimNode
	params := store.UpsertVMRuntimeParams{
		VmID:               r.VMUUID,
		CurrentNodeID:      &nodeIDCopy,
		Phase:              store.VMPhase(r.Phase),
		ObservedGeneration: obsGen,
		QEMUPID:            r.QEMUPID,
		LastStartedAt:      lastStarted,
		LastErrorMessage:   r.LastErrorMessage,
		MemoryUsedMib:      r.MemoryUsedMib,
		VMRowModRevision:   vmRev,
	}
	if err := hp.UpsertVMRuntime(ctx, params); err != nil {
		return fmt.Errorf("upsert vm_runtime: %v", err)
	}
	return nil
}

// applyHealthChecks writes the reported per-(lb, backend-VM) health verdicts,
// gated by the same placement authority as applyVMs: a node may only report
// health for VMs pinned to it (or a migration target on it). The CP stamps the
// receive time so freshness never depends on the agent's clock.
func (h *Handler) applyHealthChecks(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, reports []healthCheckReport) error {
	if len(reports) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(reports))
	for _, r := range reports {
		ids = append(ids, r.VMID)
	}
	pinned, err := hp.FilterVMIDsPinnedToNode(ctx, nodeID, ids)
	if err != nil {
		return fmt.Errorf("filter pinned vm ids (health): %v", err)
	}
	pinnedSet := make(map[uuid.UUID]struct{}, len(pinned))
	for _, id := range pinned {
		pinnedSet[id] = struct{}{}
	}
	now := time.Now().UTC()
	// Skip a soft-deleted LB so an in-flight heartbeat naming a just-deleted LB
	// does not re-create the row DeleteLoadBalancer's cascade removed (the row
	// would then leak forever - the LB is never re-listed). Resolve each distinct
	// lb_id once (few LBs per heartbeat).
	liveLB := map[uuid.UUID]bool{}
	isLive := func(lbID uuid.UUID) (bool, error) {
		if v, ok := liveLB[lbID]; ok {
			return v, nil
		}
		_, err := hp.LoadBalancerByID(ctx, lbID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				liveLB[lbID] = false
				return false, nil
			}
			return false, fmt.Errorf("resolve lb liveness: %v", err)
		}
		liveLB[lbID] = true
		return true, nil
	}
	for _, r := range reports {
		if _, ok := pinnedSet[r.VMID]; !ok {
			h.log.WarnContext(ctx, "heartbeat reports health for a vm not pinned to the reporting node; skipping",
				slog.String("node_id", nodeID.String()), slog.String("vm_uuid", r.VMID.String()))
			continue
		}
		live, err := isLive(r.LBID)
		if err != nil {
			return err
		}
		if !live {
			continue // LB deleted -> do not resurrect its health rows
		}
		if err := hp.UpsertLBBackendHealth(ctx, r.LBID, r.VMID, r.Healthy, now); err != nil {
			return fmt.Errorf("upsert lb backend health: %v", err)
		}
	}
	return nil
}

// applyPublishedListeners writes the reported per-(lb, reporting node) published
// listener bind verdicts. Unlike applyHealthChecks there is NO per-VM placement
// gate: a published listener is node-scoped (the gateway binds a public port), so
// the record is keyed by the reporting node's own identity, which the cert
// binding already authenticates. The only gate is the soft-deleted-LB skip, so an
// in-flight heartbeat naming a just-deleted LB cannot re-create a status row the
// delete cascade removed. The CP stamps the receive time so freshness never
// depends on the agent's clock.
// maxListenerErrorLen bounds the agent-supplied published-listener bind-error
// string the CP persists, so a misbehaving gateway cannot bloat etcd values.
const maxListenerErrorLen = 256

// truncateRunes returns s clipped to at most n runes (never splitting a
// multi-byte rune). n <= 0 returns "".
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

func (h *Handler) applyPublishedListeners(ctx context.Context, hp store.HeartbeatProjection, nodeID uuid.UUID, reports []publishedListenerReport) error {
	if len(reports) == 0 {
		return nil
	}
	now := time.Now().UTC()
	liveLB := map[uuid.UUID]bool{}
	isLive := func(lbID uuid.UUID) (bool, error) {
		if v, ok := liveLB[lbID]; ok {
			return v, nil
		}
		_, err := hp.LoadBalancerByID(ctx, lbID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				liveLB[lbID] = false
				return false, nil
			}
			return false, fmt.Errorf("resolve lb liveness: %v", err)
		}
		liveLB[lbID] = true
		return true, nil
	}
	for _, r := range reports {
		live, err := isLive(r.LBID)
		if err != nil {
			return err
		}
		if !live {
			continue // LB deleted -> do not resurrect its listener status rows
		}
		// The error string is agent-supplied and stored verbatim as an etcd
		// value; bound its length so a misbehaving or compromised gateway cannot
		// grow the key space with an oversized message (defense in depth - real
		// bind errors are short).
		if err := hp.UpsertLBPublishedListenerStatus(ctx, r.LBID, nodeID, r.Port, r.Bound, truncateRunes(r.Error, maxListenerErrorLen), now); err != nil {
			return fmt.Errorf("upsert lb published listener status: %v", err)
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
