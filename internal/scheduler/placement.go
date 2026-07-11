// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// PlacementRequest carries the inputs SchedulePlacement needs to pick
// a (node, pool instance) pair for a VM. PoolName must be the resolved
// pool name (the handler that owns the cluster default-pool fallback
// substitutes the cluster_settings value before calling SchedulePlacement).
// NodeHint is the optional explicit-placement override accepted by the
// VM-create endpoint as `node`; when set, the scheduler restricts the
// candidate list to exactly that node (UUID or case-insensitive name).
// VCPUs, MemoryMiB and DiskBytes drive the resource-aware fit check +
// scoring; DiskBytes is derived from the requested disk_gib in
// the single-root-disk model.
// NetworkIDs lists the cluster-wide networks the VM attaches to. When
// non-empty, the network-aware filter excludes any
// candidate node where a requested network's per-(node, network)
// reconciliation_status is not "ready" (or has no status record at all).
// Empty NetworkIDs makes the filter a no-op.
// ExcludeNodeID is the optional migration-source exclusion: a
// node-less `vm migrate` reuses the initial-placement path but must land
// on a node other than the VM's current one. When non-nil, the scheduler
// drops every candidate whose node id matches before scoring; if that
// empties the eligible set the request fails through the same
// empty-eligible diagnosis path as a fully-drained pool. Composable with
// NodeHint — exclude wins (a hint pinning the excluded node empties the
// set rather than returning the source).
type PlacementRequest struct {
	PoolName      string
	NodeHint      *string
	ExcludeNodeID *uuid.UUID
	VCPUs         int
	MemoryMiB     int
	DiskBytes     int64
	NetworkIDs    []uuid.UUID
}

// PlacementDecision is the scheduler's verdict: the chosen node row
// and the pool-instance row on that node. Callers use PoolInstance.ID
// as the storage_pool_id for the VM-create transaction and Node.ID as
// the pinned_node_id.
//
// Both `Node` and `PoolInstance` carry view-backed rows — Node from
// node_effective_availability, PoolInstance from pool_effective_capacity
// — so post-decision callers can inspect either raw heartbeat / scan
// columns or the effective columns with pending-VM accounting.
type PlacementDecision struct {
	Node         store.NodeEffectiveAvailability
	PoolInstance store.PoolEffectiveCapacity
}

// PlacementConfig groups the placement-time knobs SchedulePlacement
// consumes. Algorithm selects the scoring algorithm; Resources gates
// which dimensions participate in fit checks + scoring and assigns
// per-resource overcommit ratios. Empty Algorithm defaults to
// AlgorithmResourceAware. Zero-value Resources disables every resource
// and degrades scoring to count-based — handler wire-up uses
// `defaultResourcesConfig` so zero-value only surfaces in tests.
type PlacementConfig struct {
	Algorithm string
	Resources ResourcesConfig
}

// ResourcesConfig is the scheduler-internal mirror of
// config.ResourcesConfig (internal/config/api.go). Keeping a dedicated
// type at the scheduler layer keeps the leaf-package free of a cycle
// into internal/config; the api-server copies fields at construction
// time.
type ResourcesConfig struct {
	CPU    ResourceConfig
	Memory MemoryResourceConfig
	Disk   ResourceConfig
}

// MemoryResourceConfig mirrors config.MemoryResourceConfig at the scheduler
// layer (internal/scheduler is a leaf package and must not import
// internal/config). The api-server copies fields via
// SchedulerResourcesFromConfig at construction time.
type MemoryResourceConfig struct {
	Enabled                  bool
	OvercommitRatio          float64
	OvercommitZramFloorMib   int64
	OvercommitZramConfidence float64
}

// ResourceConfig pins one resource dimension's placement contribution.
// Enabled toggles inclusion in the fit check + score; OvercommitRatio
// multiplies effective availability (values > 1.0 enable overcommit,
// values < 1.0 reserve headroom). Both forwarded verbatim from
// config.ResourceConfig.
type ResourceConfig struct {
	Enabled         bool
	OvercommitRatio float64
}

// Algorithm names accepted by PlacementConfig.Algorithm. Unknown values
// surface as a startup-time config error (APIConfig.Validate) so the
// hot path never has to handle an invalid setting.
const (
	// AlgorithmResourceAware ranks candidates by post-placement
	// LeastAllocated scoring averaged across enabled resources (CPU /
	// memory / disk). Nodes whose heartbeat / scan metrics have not
	// landed yet fall back to count-based scoring for that individual
	// candidate (mixed fleets supported).
	AlgorithmResourceAware = "resource_aware"

	// AlgorithmLeastVMCount preserves the pre-resource-aware
	// algorithm — minimum pinned-VM count wins. Kept as an operator-
	// configurable opt-out for clusters that prefer count-based
	// spread or whose heartbeat metrics are unreliable. Both paths use
	// the same name lexicographic tie-break.
	AlgorithmLeastVMCount = "least_vm_count"
)

// Sentinel errors. SchedulePlacement wraps these with %w so handlers
// can route them to envelope codes via errors.Is.
var (
	// ErrPoolNotFound — no pool instance with this name exists on any
	// node, or every instance lives on a node that is not eligible
	// for scheduling (soft-deleted, not-ready, cordoned). Handler maps
	// to 404 pool_not_found.
	ErrPoolNotFound = errors.New("pool not found in cluster")

	// ErrPoolNotOnNode — NodeHint was provided but the named node has
	// no instance of the requested pool. Handler maps to 409
	// pool_not_on_node.
	ErrPoolNotOnNode = errors.New("pool not present on requested node")

	// ErrNoEligibleNodes — at least one pool instance exists in the
	// raw schema, but the eligibility filter (ready, not-cordoned)
	// excluded every candidate. Distinct from ErrPoolNotFound so the
	// operator can tell "this pool doesn't exist" from "the cluster is
	// drained". Handler maps to 409 no_eligible_nodes.
	ErrNoEligibleNodes = errors.New("no eligible nodes available")

	// ErrInsufficientResources — pool instances exist on eligible
	// nodes, but no node has enough free CPU / memory / disk to
	// accommodate the request. Wrapped error carries a
	// *InsufficientResourcesDetail accessible via errors.As; the
	// handler renders it into the 409 no_eligible_nodes envelope's
	// `details` payload so operators see what each candidate had on
	// hand.
	ErrInsufficientResources = errors.New("no eligible node has sufficient resources")

	// ErrDefaultPoolNotSet — surfaced not by SchedulePlacement itself
	// but defined here for callers (the VM-create handler) so all
	// placement-related sentinels live together. Handler maps to 400
	// default_pool_not_set.
	ErrDefaultPoolNotSet = errors.New("cluster default pool not set")

	// ErrNodeHintIsUUID — NodeHint accepts a node name only. A UUID
	// literal in the request body's `node` field surfaces this sentinel
	// so the handler can emit the canonical 400 validation_failed
	// envelope with uuid-rejection details.
	ErrNodeHintIsUUID = errors.New("node hint must be a name, not a uuid")

	// ErrUnknownAlgorithm — defensive guard on the hot path; the
	// startup-time config validation prevents this from being reached
	// in production. Surfaces as 500 internal.
	ErrUnknownAlgorithm = errors.New("unknown placement algorithm")
)

// NodeUtilization is a single-node snapshot rendered into the
// insufficient-resources error response. Operators read this to see
// which candidates were considered and why none fit. Nodes are
// referenced by name. The pool name + disk fields are included
// because disk capacity is per-pool, not per-node, so the rendered
// payload pins down the exact (node, pool) pair that fell short.
type NodeUtilization struct {
	Node           string `json:"node"`
	Pool           string `json:"pool"`
	CPUUsed        int32  `json:"cpu_used"`
	CPUTotal       int32  `json:"cpu_total"`
	MemUsedMiB     int64  `json:"mem_used_mib"`
	MemTotalMiB    int64  `json:"mem_total_mib"`
	DiskUsedBytes  int64  `json:"disk_used_bytes"`
	DiskTotalBytes int64  `json:"disk_total_bytes"`
}

// InsufficientResourcesDetail is the structured payload attached to
// ErrInsufficientResources. Callers extract it via errors.As and copy the
// fields into the wire envelope's `details` map. The detail keeps the
// rendered NodeUtilization slice plus the required-vs-considered
// summary so the operator can spot the smallest-fit gap.
type InsufficientResourcesDetail struct {
	RequiredCPU          int32
	RequiredMemMiB       int32
	RequiredDiskBytes    int64
	CandidatesConsidered int
	CandidatesEligible   int
	NodeUtilization      []NodeUtilization
}

// insufficientResourcesError attaches an InsufficientResourcesDetail to
// ErrInsufficientResources via the standard error-wrap chain. Handlers
// extract the detail via ExtractInsufficientResources rather than poking
// at this type directly — keeps the error implementation private.
type insufficientResourcesError struct {
	Detail InsufficientResourcesDetail
}

func (e *insufficientResourcesError) Error() string {
	return fmt.Sprintf("scheduler: %s: required cpu=%d mem_mib=%d disk_bytes=%d, considered=%d eligible=%d",
		ErrInsufficientResources.Error(), e.Detail.RequiredCPU, e.Detail.RequiredMemMiB,
		e.Detail.RequiredDiskBytes, e.Detail.CandidatesConsidered, e.Detail.CandidatesEligible)
}

func (e *insufficientResourcesError) Unwrap() error { return ErrInsufficientResources }

// ExtractInsufficientResources walks the error chain looking for a
// resource-shortage payload. Returns (detail, true) when the chain
// carries one and (nil, true) when ErrInsufficientResources is matched
// but the structured detail is unreachable (defensive — production
// paths always attach a detail). Returns (nil, false) otherwise so
// the caller can fall through to other sentinel checks.
func ExtractInsufficientResources(err error) (*InsufficientResourcesDetail, bool) {
	if !errors.Is(err, ErrInsufficientResources) {
		return nil, false
	}
	var wrap *insufficientResourcesError
	if errors.As(err, &wrap) && wrap != nil {
		d := wrap.Detail
		return &d, true
	}
	return nil, true
}

// PressuredNode is a single-entry in the `filtered_due_to_pressure`
// payload. Nodes / pools are referenced by name. `Pool` is non-empty
// when the pressure is pool-scoped (disk_pressure) and empty for
// node-scoped conditions (memory_pressure, system_disk_pressure).
// `Conditions` enumerates every active pressure
// flag on the entry — a node with both memory + system_disk pressure
// emits a single entry with two conditions[]. Per-condition timestamps
// are optional pointers — present only when their condition is in
// `Conditions`.
type PressuredNode struct {
	Node                    string     `json:"node"`
	Pool                    string     `json:"pool,omitempty"`
	Conditions              []string   `json:"conditions"`
	MemoryPressureSince     *time.Time `json:"memory_pressure_since,omitempty"`
	SystemDiskPressureSince *time.Time `json:"system_disk_pressure_since,omitempty"`
	DiskPressureSince       *time.Time `json:"disk_pressure_since,omitempty"`
}

// NodePressureDetail is the structured payload attached to
// ErrNoEligibleNodes when at least one pool instance was excluded from
// placement specifically by an active pressure condition. Distinct from
// InsufficientResourcesDetail because the operator action is different:
// pressure-filtered nodes recover automatically (or never, if
// hardware is undersized), while insufficient resources call for a
// larger VM-create request, more capacity, or a different pool.
type NodePressureDetail struct {
	Nodes []PressuredNode
}

// nodePressureError wraps ErrNoEligibleNodes c a structured pressure
// payload. Surfaced by the handler edge via ExtractNodePressureDetail
// → 409 no_eligible_nodes envelope with `details.reason="node_pressure"`
// and `details.filtered_due_to_pressure`.
type nodePressureError struct {
	Detail NodePressureDetail
}

func (e *nodePressureError) Error() string {
	return fmt.Sprintf("scheduler: %s: %d node(s) under pressure",
		ErrNoEligibleNodes.Error(), len(e.Detail.Nodes))
}

func (e *nodePressureError) Unwrap() error { return ErrNoEligibleNodes }

// ExtractNodePressureDetail walks the error chain looking for a
// pressure-filtered payload. Returns (detail, true) when the chain
// carries one. Returns (nil, false) otherwise so the caller can fall
// through to the bare ErrNoEligibleNodes envelope.
func ExtractNodePressureDetail(err error) (*NodePressureDetail, bool) {
	var wrap *nodePressureError
	if errors.As(err, &wrap) && wrap != nil {
		d := wrap.Detail
		return &d, true
	}
	return nil, false
}

// UnreadyNetwork names one requested network that excluded a node, with
// the per-(node, network) reconciliation status that disqualified it.
// Status is "failed", "pending", or "missing" (no status record at all —
// the agent has not yet reported the network on this node). The
// network is referenced by id; the handler edge resolves names for the
// envelope when it has them.
type UnreadyNetwork struct {
	NetworkID uuid.UUID `json:"network_id"`
	Status    string    `json:"status"`
}

// NetworkUnreadyNode is a single entry in the network-unready payload:
// one excluded node plus every requested network that was not ready on
// it. Nodes are referenced by name, consistent with the other detail
// shapes.
type NetworkUnreadyNode struct {
	Node     string           `json:"node"`
	Networks []UnreadyNetwork `json:"networks"`
}

// NetworkUnreadyDetail is the structured payload attached to
// ErrNoEligibleNodes when at least one candidate that otherwise fit the
// request was excluded specifically because a requested network had not
// reconciled to "ready" on it. Distinct from
// NodePressureDetail and InsufficientResourcesDetail because the
// operator action differs: a failed network reconciliation calls for
// inspecting the agent's network materialisation, not capacity or
// pressure.
type NetworkUnreadyDetail struct {
	Nodes []NetworkUnreadyNode
}

// networkUnreadyError wraps ErrNoEligibleNodes with a structured
// network-unready payload. Surfaced by the handler edge via
// ExtractNetworkUnreadyDetail.
type networkUnreadyError struct {
	Detail NetworkUnreadyDetail
}

func (e *networkUnreadyError) Error() string {
	return fmt.Sprintf("scheduler: %s: %d node(s) with an unready requested network",
		ErrNoEligibleNodes.Error(), len(e.Detail.Nodes))
}

func (e *networkUnreadyError) Unwrap() error { return ErrNoEligibleNodes }

// ExtractNetworkUnreadyDetail walks the error chain looking for a
// network-unready payload. Returns (detail, true) when the chain carries
// one. Returns (nil, false) otherwise so the caller can fall through to
// the other ErrNoEligibleNodes detail shapes.
func ExtractNetworkUnreadyDetail(err error) (*NetworkUnreadyDetail, bool) {
	var wrap *networkUnreadyError
	if errors.As(err, &wrap) && wrap != nil {
		d := wrap.Detail
		return &d, true
	}
	return nil, false
}

// Querier is the subset of store.Querier SchedulePlacement reads from.
// Keeping the surface narrow lets unit tests pass an in-memory fake.
//
// The three `List*PressuredCandidatesByName` / `ListDiskPressuredPoolsByName`
// queries are diagnostic-only. They run from the failure branch when
// the regular eligibility list comes back empty but the pool exists
// somewhere. Their combined hit list populates the
// `filtered_due_to_pressure` envelope payload — one entry per
// (node, pool) with every active condition aggregated.
type Querier interface {
	ListEligiblePoolsByName(ctx context.Context, name string) ([]store.ListEligiblePoolsByNameRow, error)
	ListMemoryPressuredCandidatesByName(ctx context.Context, name string) ([]store.ListMemoryPressuredCandidatesByNameRow, error)
	ListSystemDiskPressuredCandidatesByName(ctx context.Context, name string) ([]store.ListSystemDiskPressuredCandidatesByNameRow, error)
	ListDiskPressuredPoolsByName(ctx context.Context, name string) ([]store.ListDiskPressuredPoolsByNameRow, error)
	ListStoragePoolsByName(ctx context.Context, name string) ([]store.StoragePool, error)
	CountRunningVMsByNode(ctx context.Context, nodeID *uuid.UUID) (int64, error)
	// ListNetworkNodeStatusByNode returns every per-network reconciliation
	// record owned by the node. The network-aware filter reads it once per
	// surviving candidate when the request names at least one network.
	ListNetworkNodeStatusByNode(ctx context.Context, nodeID uuid.UUID) ([]store.NetworkNodeStatus, error)
}

// SchedulePlacement picks a node + pool instance for a VM create.
//
// Algorithm (resource-aware, default):
//  1. Enumerate eligible (pool instance, node) tuples for PoolName.
//     Eligibility is "node is ready and not cordoned" — encoded in the
//     SQL query, not re-derived here. The query reads from the
//     pool_effective_capacity + node_effective_availability views so
//     each candidate carries effective-availability columns with
//     pending-VM accounting.
//  2. If NodeHint is set, narrow the list to the matching node by
//     case-insensitive name. UUID literals reject early.
//  3. Resource fit check: a node fits if its effective availability
//     for each enabled resource (CPU, memory, disk) meets the request,
//     multiplied by the per-resource OvercommitRatio. Resources with
//     Enabled=false drop out of the check. Nodes / pools that are
//     missing the underlying metric columns fall back to count-based
//     scoring for that candidate (graceful at cluster bootstrap).
//  4. Score by configured algorithm. resource_aware ranks by
//     LeastAllocated across enabled resources: lower post-placement
//     utilization wins. least_vm_count preserves the count-based
//     algorithm for operators who prefer count-based spread.
//  5. Tie-break by node name lexicographic (lowercase) — operator-
//     observable identifier consistent with the name-based reference
//     model. Both algorithm paths use the same tie-break.
func SchedulePlacement(ctx context.Context, q Querier, req PlacementRequest, cfg PlacementConfig) (PlacementDecision, error) {
	if req.PoolName == "" {
		return PlacementDecision{}, fmt.Errorf("scheduler: pool name required: %w", ErrPoolNotFound)
	}

	algorithm := cfg.Algorithm
	if algorithm == "" {
		algorithm = AlgorithmResourceAware
	}

	eligible, err := q.ListEligiblePoolsByName(ctx, req.PoolName)
	if err != nil {
		return PlacementDecision{}, fmt.Errorf("scheduler: list eligible pools: %w", err)
	}

	if len(eligible) == 0 {
		return PlacementDecision{}, diagnoseEmptyEligible(ctx, q, req.PoolName)
	}

	if req.NodeHint != nil {
		filtered, hintErr := filterByNodeHint(eligible, *req.NodeHint)
		if hintErr != nil {
			return PlacementDecision{}, fmt.Errorf("scheduler: node hint %q: %w", *req.NodeHint, hintErr)
		}
		eligible = filtered
	}

	// Migration source exclusion. Drop the current node so a
	// node-less migrate re-pins elsewhere. Exclude wins over NodeHint: a
	// hint pinning the excluded node empties the set here rather than
	// returning the source. An emptied set routes through the same
	// empty-eligible diagnosis path the zero-candidates case uses, so the
	// caller gets a retryable no-eligible-target result, not a panic.
	if req.ExcludeNodeID != nil {
		eligible = excludeNode(eligible, *req.ExcludeNodeID)
		if len(eligible) == 0 {
			return PlacementDecision{}, diagnoseEmptyEligible(ctx, q, req.PoolName)
		}
	}

	considered := len(eligible)
	fits, rejected := filterByResources(eligible, req, cfg.Resources)
	if len(fits) == 0 {
		// Every candidate exists but none fit. Build a structured
		// utilization payload so the operator can see what each node
		// had on hand. NodeHint paths land here too — a hint-matched
		// node that cannot fit the request gets the same envelope.
		detail := InsufficientResourcesDetail{
			RequiredCPU:          clampInt32(req.VCPUs),
			RequiredMemMiB:       clampInt32(req.MemoryMiB),
			RequiredDiskBytes:    req.DiskBytes,
			CandidatesConsidered: considered,
			CandidatesEligible:   0,
			NodeUtilization:      renderUtilization(rejected),
		}
		return PlacementDecision{}, fmt.Errorf("scheduler: pool %q: %w", req.PoolName, &insufficientResourcesError{Detail: detail})
	}

	fits, err = applyNetworkFilter(ctx, q, fits, req)
	if err != nil {
		return PlacementDecision{}, err
	}

	scored, err := scoreCandidates(ctx, q, fits, req, cfg, algorithm)
	if err != nil {
		return PlacementDecision{}, fmt.Errorf("scheduler: score candidates: %w", err)
	}
	winner := pickWinner(scored)
	return PlacementDecision{
		Node:         winner.row.NodeEffectiveAvailability,
		PoolInstance: winner.row.PoolEffectiveCapacity,
	}, nil
}

// diagnoseEmptyEligible runs when the eligibility query returned nothing.
// It distinguishes "pool name not in cluster at all" (ErrPoolNotFound,
// 404) from "pool exists but every host is cordoned / unreachable /
// pressured" (ErrNoEligibleNodes, 409). A separate ListStoragePoolsByName
// lookup is cheap and the diagnostic is worth it. When the pool exists
// somewhere it runs the three pressure-diagnostic queries and combines
// their hits into a single NodePressureDetail so the operator has
// something actionable even when only one pressure type fired.
func diagnoseEmptyEligible(ctx context.Context, q Querier, poolName string) error {
	pools, listErr := q.ListStoragePoolsByName(ctx, poolName)
	if listErr != nil {
		return fmt.Errorf("scheduler: list pools: %w", listErr)
	}
	if len(pools) == 0 {
		return fmt.Errorf("scheduler: pool %q: %w", poolName, ErrPoolNotFound)
	}
	mem, mErr := q.ListMemoryPressuredCandidatesByName(ctx, poolName)
	if mErr != nil {
		return fmt.Errorf("scheduler: list memory-pressured candidates: %w", mErr)
	}
	sysDisk, sErr := q.ListSystemDiskPressuredCandidatesByName(ctx, poolName)
	if sErr != nil {
		return fmt.Errorf("scheduler: list system-disk-pressured candidates: %w", sErr)
	}
	poolDisk, dErr := q.ListDiskPressuredPoolsByName(ctx, poolName)
	if dErr != nil {
		return fmt.Errorf("scheduler: list disk-pressured pools: %w", dErr)
	}
	if len(mem)+len(sysDisk)+len(poolDisk) > 0 {
		detail := buildNodePressureDetail(mem, sysDisk, poolDisk)
		return fmt.Errorf("scheduler: pool %q has no eligible nodes (pressure): %w", poolName, &nodePressureError{Detail: detail})
	}
	return fmt.Errorf("scheduler: pool %q has no eligible nodes: %w", poolName, ErrNoEligibleNodes)
}

// filterByNodeHint narrows the candidate list to the row whose node
// matches the hint by case-insensitive name. UUID literals are
// rejected with ErrNodeHintIsUUID before any scan; the handler maps that
// to the 400 validation_failed envelope. Empty narrowed list →
// ErrPoolNotOnNode; the handler emits 409 pool_not_on_node.
func filterByNodeHint(rows []store.ListEligiblePoolsByNameRow, hint string) ([]store.ListEligiblePoolsByNameRow, error) {
	if _, err := uuid.Parse(hint); err == nil {
		return nil, ErrNodeHintIsUUID
	}
	for _, r := range rows {
		if strings.EqualFold(r.NodeEffectiveAvailability.Name, hint) {
			return []store.ListEligiblePoolsByNameRow{r}, nil
		}
	}
	return nil, ErrPoolNotOnNode
}

// excludeNode drops every candidate whose node id matches excludeID,
// preserving input order. It backs the migration-source exclusion:
// a node-less `vm migrate` reuses the placement path but must land
// somewhere other than the VM's current node. An empty result is the
// caller's signal to route through diagnoseEmptyEligible.
func excludeNode(rows []store.ListEligiblePoolsByNameRow, excludeID uuid.UUID) []store.ListEligiblePoolsByNameRow {
	out := rows[:0:0]
	for _, r := range rows {
		if r.NodeEffectiveAvailability.ID == excludeID {
			continue
		}
		out = append(out, r)
	}
	return out
}

// candidate carries the per-row scheduler state through the pipeline.
// useCountFallback flips to true when the node has not reported
// heartbeat metrics yet — in that case the resource-aware path scores
// the candidate by pinned-VM count instead of utilization. Mixed
// fleets (some metric-bearing nodes, some not) are supported.
type candidate struct {
	row              store.ListEligiblePoolsByNameRow
	useCountFallback bool
}

// scoredCandidate adds the comparator score onto a candidate. Lower
// score = better fit. The fallback path produces a large numeric
// value so any metric-bearing candidate wins ahead of a fallback one
// at equal load — the operator should see resource-aware placement
// kick in as soon as heartbeats arrive.
type scoredCandidate struct {
	candidate
	score float64
}

// filterByResources runs the fit check on the eligible list. Per-resource
// gating: each enabled resource (CPU, memory, disk) must individually
// satisfy the request before the candidate is kept. OvercommitRatio
// multiplies the effective availability so values > 1.0 inflate
// capacity, values < 1.0 reserve headroom. Candidates with any required
// metric column missing fall back to count-based scoring (kept in fits
// with useCountFallback=true) — preserves the bootstrap-friendly behaviour.
// Returned rejected slice feeds the insufficient-
// resources error payload.
func filterByResources(rows []store.ListEligiblePoolsByNameRow, req PlacementRequest, cfg ResourcesConfig) (fits, rejected []candidate) {
	for _, r := range rows {
		c := candidate{row: r}
		if !candidateHasMetrics(r, req, cfg) {
			c.useCountFallback = true
			fits = append(fits, c)
			continue
		}
		if !fitsAllResources(r, req, cfg) {
			rejected = append(rejected, c)
			continue
		}
		fits = append(fits, c)
	}
	return fits, rejected
}

// applyNetworkFilter runs the network-aware placement gate when the
// request names at least one network and returns the surviving
// candidates. An empty NetworkIDs makes it a pass-through (the network-
// status query is never consulted). When every candidate is excluded it
// returns an ErrNoEligibleNodes chain carrying the NetworkUnreadyDetail;
// a Querier failure is wrapped verbatim. Keeping the branching here
// keeps SchedulePlacement's cyclomatic complexity in check.
func applyNetworkFilter(ctx context.Context, q Querier, fits []candidate, req PlacementRequest) ([]candidate, error) {
	if len(req.NetworkIDs) == 0 {
		return fits, nil
	}
	survivors, detail, err := filterByNetworkReadiness(ctx, q, fits, req.NetworkIDs)
	if err != nil {
		return nil, fmt.Errorf("scheduler: filter by network readiness: %w", err)
	}
	if len(survivors) == 0 {
		return nil, fmt.Errorf("scheduler: pool %q has no node with every requested network ready: %w", req.PoolName, &networkUnreadyError{Detail: detail})
	}
	return survivors, nil
}

// filterByNetworkReadiness applies the network-aware placement filter.
// For each candidate that already passed the resource
// fit, it loads the node's per-(node, network) reconciliation records and
// keeps the candidate only when every requested network id is present
// with status "ready". A network that is missing, "pending", or "failed"
// excludes the node; the rejected node and its offending networks are
// accumulated into a NetworkUnreadyDetail so the no-eligible error can
// explain which node failed on which network. Candidate order is
// preserved in both the survivor and rejected lists. Caller guarantees
// len(networkIDs) > 0.
func filterByNetworkReadiness(ctx context.Context, q Querier, fits []candidate, networkIDs []uuid.UUID) (survivors []candidate, detail NetworkUnreadyDetail, err error) {
	for _, c := range fits {
		nodeID := c.row.NodeEffectiveAvailability.ID
		statuses, listErr := q.ListNetworkNodeStatusByNode(ctx, nodeID)
		if listErr != nil {
			return nil, NetworkUnreadyDetail{}, fmt.Errorf("list network status for node %s: %w", nodeID, listErr)
		}
		byNetwork := make(map[uuid.UUID]string, len(statuses))
		for _, st := range statuses {
			byNetwork[st.NetworkID] = st.ReconciliationStatus
		}
		var unready []UnreadyNetwork
		for _, netID := range networkIDs {
			status, ok := byNetwork[netID]
			if !ok {
				unready = append(unready, UnreadyNetwork{NetworkID: netID, Status: "missing"})
				continue
			}
			if status != networkStatusReady {
				unready = append(unready, UnreadyNetwork{NetworkID: netID, Status: status})
			}
		}
		if len(unready) > 0 {
			detail.Nodes = append(detail.Nodes, NetworkUnreadyNode{
				Node:     c.row.NodeEffectiveAvailability.Name,
				Networks: unready,
			})
			continue
		}
		survivors = append(survivors, c)
	}
	return survivors, detail, nil
}

// networkStatusReady is the per-(node, network) reconciliation_status a
// candidate must report for every requested network before the
// network-aware filter admits it.
const networkStatusReady = "ready"

// fitsAllResources walks the per-resource gate. Effective columns are
// guaranteed non-nil by candidateHasMetrics for every enabled resource,
// so the deref is safe. For disk specifically `req.DiskBytes <= 0`
// short-circuits the check — a handler that does not derive a disk
// requirement (e.g. tests, future per-resource-disabled flows) skips
// disk regardless of cfg.Disk.Enabled.
func fitsAllResources(r store.ListEligiblePoolsByNameRow, req PlacementRequest, cfg ResourcesConfig) bool {
	if cfg.CPU.Enabled {
		avail := float64(*r.NodeEffectiveAvailability.CPUCoresEffective) * cfg.CPU.OvercommitRatio
		if avail < float64(req.VCPUs) {
			return false
		}
	}
	if cfg.Memory.Enabled {
		avail := float64(*r.NodeEffectiveAvailability.MemoryEffectiveMib) * cfg.Memory.OvercommitRatio
		if avail < float64(req.MemoryMiB) {
			return false
		}
	}
	if cfg.Disk.Enabled && req.DiskBytes > 0 {
		avail := float64(*r.PoolEffectiveCapacity.AvailableBytesEffective) * cfg.Disk.OvercommitRatio
		if avail < float64(req.DiskBytes) {
			return false
		}
	}
	return true
}

// candidateHasMetrics reports whether the candidate carries every
// metric column the configured fit check + scoring path will read.
// Per-resource gating: only enabled resources are required to have
// their underlying columns set. A node without heartbeat metrics fails
// the CPU / memory checks; a pool without last-scan numbers fails the
// disk check. Partial nils — a node reporting CPU but not memory —
// are treated as missing metrics, not "zero available", consistent
// with the bootstrap-fallback semantic.
func candidateHasMetrics(r store.ListEligiblePoolsByNameRow, req PlacementRequest, cfg ResourcesConfig) bool {
	n := r.NodeEffectiveAvailability
	if cfg.CPU.Enabled {
		if n.CPUCoresTotal == nil || n.CPUCoresAvailable == nil || n.CPUCoresEffective == nil {
			return false
		}
	}
	if cfg.Memory.Enabled {
		if n.MemoryTotalMib == nil || n.MemoryAvailableMib == nil || n.MemoryEffectiveMib == nil {
			return false
		}
	}
	if cfg.Disk.Enabled && req.DiskBytes > 0 {
		p := r.PoolEffectiveCapacity
		if p.CapacityBytes == nil || p.AvailableBytes == nil || p.AvailableBytesEffective == nil {
			return false
		}
	}
	return true
}

// renderUtilization builds the NodeUtilization slice for the
// insufficient-resources detail. Used cores / memory / disk are
// computed against the effective columns so the envelope reflects the
// scheduler's actual view (including pending VMs / disks) rather than
// the raw heartbeat / scan snapshot — operators reading "8/8 used"
// should see why their request didn't fit, not the agent's stale
// numbers. Nodes / pools missing metric columns surface as zeros
// across the board — they have nothing to report. Order preserves
// the input ordering (SQL `order by n.id`).
func renderUtilization(rejected []candidate) []NodeUtilization {
	out := make([]NodeUtilization, 0, len(rejected))
	for _, c := range rejected {
		n := c.row.NodeEffectiveAvailability
		p := c.row.PoolEffectiveCapacity
		var cpuTotal, cpuEff int32
		var memTotal, memEff int64
		var diskTotal, diskEff int64
		if n.CPUCoresTotal != nil {
			cpuTotal = *n.CPUCoresTotal
		}
		if n.CPUCoresEffective != nil {
			cpuEff = *n.CPUCoresEffective
		}
		if n.MemoryTotalMib != nil {
			memTotal = *n.MemoryTotalMib
		}
		if n.MemoryEffectiveMib != nil {
			memEff = *n.MemoryEffectiveMib
		}
		if p.CapacityBytes != nil {
			diskTotal = *p.CapacityBytes
		}
		if p.AvailableBytesEffective != nil {
			diskEff = *p.AvailableBytesEffective
		}
		diskUsed := diskTotal - diskEff
		if diskUsed < 0 {
			diskUsed = 0
		}
		out = append(out, NodeUtilization{
			Node:           n.Name,
			Pool:           p.Name,
			CPUUsed:        cpuTotal - cpuEff,
			CPUTotal:       cpuTotal,
			MemUsedMiB:     memTotal - memEff,
			MemTotalMiB:    memTotal,
			DiskUsedBytes:  diskUsed,
			DiskTotalBytes: diskTotal,
		})
	}
	return out
}

// scoreCandidates dispatches to the per-algorithm scorer. Unknown
// algorithm values reach here only if the startup-time config check
// was bypassed; surfacing ErrUnknownAlgorithm keeps the handler edge
// honest.
func scoreCandidates(ctx context.Context, q Querier, fits []candidate, req PlacementRequest, cfg PlacementConfig, algorithm string) ([]scoredCandidate, error) {
	switch algorithm {
	case AlgorithmResourceAware:
		return scoreResourceAware(ctx, q, fits, req, cfg.Resources)
	case AlgorithmLeastVMCount:
		return scoreLeastVMCount(ctx, q, fits)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownAlgorithm, algorithm)
	}
}

// scoreResourceAware applies the LeastAllocated formula averaged across
// enabled resources. Effective columns are read instead of raw heartbeat
// / scan columns so VMs / disks pinned after the last observation
// contribute to the utilization estimate. Nodes / pools without metrics
// fall back to a count-derived score scaled by a large multiplier so
// metric-bearing candidates win ahead of fallback ones whenever both
// fit. All-resources-disabled also degrades to count-based scoring
// (Least-VM-count parity without switching algorithms).
func scoreResourceAware(ctx context.Context, q Querier, fits []candidate, req PlacementRequest, cfg ResourcesConfig) ([]scoredCandidate, error) {
	out := make([]scoredCandidate, 0, len(fits))
	for _, c := range fits {
		if c.useCountFallback {
			nodeID := c.row.NodeEffectiveAvailability.ID
			count, err := q.CountRunningVMsByNode(ctx, &nodeID)
			if err != nil {
				return nil, fmt.Errorf("count running vms on node %s: %w", nodeID, err)
			}
			// Fallback score sits well above the resource-aware
			// scoring range (which lives in [0, 1]) so any candidate
			// with real metrics outranks a fallback one at equal load.
			out = append(out, scoredCandidate{candidate: c, score: 1.0 + float64(count)})
			continue
		}
		score, factors := utilizationScore(c.row, req, cfg)
		if factors == 0 {
			// Every resource disabled (or disk skipped because
			// DiskBytes=0 with cfg.Disk.Enabled=false elsewhere): the
			// candidate has no utilization signal to score against,
			// so fall back to count-based ranking. Equivalent to
			// least_vm_count for this candidate but without switching the
			// whole cluster algorithm — operators can disable every
			// resource for "pure count" behaviour ad hoc.
			nodeID := c.row.NodeEffectiveAvailability.ID
			count, err := q.CountRunningVMsByNode(ctx, &nodeID)
			if err != nil {
				return nil, fmt.Errorf("count running vms on node %s: %w", nodeID, err)
			}
			out = append(out, scoredCandidate{candidate: c, score: 1.0 + float64(count)})
			continue
		}
		out = append(out, scoredCandidate{candidate: c, score: score / float64(factors)})
	}
	return out, nil
}

// utilizationScore returns the sum of per-resource post-placement
// utilization terms plus the number of factors contributing. The
// caller divides by `factors` for the averaged LeastAllocated score.
// Disabled resources do not contribute (the candidate's average stays
// honest when fewer dimensions matter).
func utilizationScore(r store.ListEligiblePoolsByNameRow, req PlacementRequest, cfg ResourcesConfig) (sum float64, factors int) {
	n := r.NodeEffectiveAvailability
	p := r.PoolEffectiveCapacity
	if cfg.CPU.Enabled && n.CPUCoresTotal != nil && n.CPUCoresEffective != nil {
		sum += computeUtil(int64(*n.CPUCoresTotal), int64(*n.CPUCoresEffective), int64(req.VCPUs), cfg.CPU.OvercommitRatio)
		factors++
	}
	if cfg.Memory.Enabled && n.MemoryTotalMib != nil && n.MemoryEffectiveMib != nil {
		sum += computeUtil(*n.MemoryTotalMib, *n.MemoryEffectiveMib, int64(req.MemoryMiB), cfg.Memory.OvercommitRatio)
		factors++
	}
	if cfg.Disk.Enabled && req.DiskBytes > 0 && p.CapacityBytes != nil && p.AvailableBytesEffective != nil {
		sum += computeUtil(*p.CapacityBytes, *p.AvailableBytesEffective, req.DiskBytes, cfg.Disk.OvercommitRatio)
		factors++
	}
	return sum, factors
}

// computeUtil returns post-placement utilization for one resource
// dimension: (used + need) / (total * ratio). Effective availability
// already excludes pending VMs / disks, so `used = total - effective`
// captures observed-plus-pending consumption. Floored at zero to guard
// against view-projection edge cases (real schema invariants prevent
// negative used values). Pure function — keeps the score loop testable
// in isolation from Querier interactions.
func computeUtil(total, effective, need int64, ratio float64) float64 {
	if total <= 0 || ratio <= 0 {
		return 0
	}
	used := total - effective
	if used < 0 {
		used = 0
	}
	return float64(used+need) / (float64(total) * ratio)
}

// scoreLeastVMCount implements the count-based algorithm: minimum
// pinned-VM count wins. CountRunningVMsByNode runs once per candidate
// — fine for the anticipated fleet sizes (< 100 nodes); a future
// revision may batch this into a single query when nodes climb past
// that range.
func scoreLeastVMCount(ctx context.Context, q Querier, fits []candidate) ([]scoredCandidate, error) {
	out := make([]scoredCandidate, 0, len(fits))
	for _, c := range fits {
		nodeID := c.row.NodeEffectiveAvailability.ID
		count, err := q.CountRunningVMsByNode(ctx, &nodeID)
		if err != nil {
			return nil, fmt.Errorf("count running vms on node %s: %w", nodeID, err)
		}
		out = append(out, scoredCandidate{candidate: c, score: float64(count)})
	}
	return out, nil
}

// pickWinner stable-sorts by (score ascending, name lexicographic
// ascending — lowercase). The lowercase comparison matches the
// uq_nodes_name functional UNIQUE index; both algorithm paths use
// this tie-break.
func pickWinner(scored []scoredCandidate) scoredCandidate {
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score < scored[j].score
		}
		return strings.ToLower(scored[i].row.NodeEffectiveAvailability.Name) <
			strings.ToLower(scored[j].row.NodeEffectiveAvailability.Name)
	})
	return scored[0]
}

// buildNodePressureDetail merges the three diagnostic-query results
// into a deduplicated NodePressureDetail. Memory + system_disk are
// node-scoped, disk is pool-scoped. Entries key on (node, pool) so:
//
//   - A node with both memory + system_disk pressure → ONE entry,
//     pool="", conditions=[memory_pressure, system_disk_pressure].
//   - A pool with disk_pressure → ONE entry, pool=<name>, conditions=
//     [disk_pressure]. Distinct from any node-scoped entry that might
//     reference the same node — operator wants to see that the pool
//     itself is exhausted regardless from node health.
//   - Same node hosting two pools, both disk-pressured → TWO entries
//     (one per pool).
//
// Ordering is preserved per source query (node id asc per the SQL),
// which gives a stable presentation order across runs.
func buildNodePressureDetail(
	mem []store.ListMemoryPressuredCandidatesByNameRow,
	sysDisk []store.ListSystemDiskPressuredCandidatesByNameRow,
	poolDisk []store.ListDiskPressuredPoolsByNameRow,
) NodePressureDetail {
	type key struct {
		node string
		pool string
	}
	index := make(map[key]*PressuredNode)
	order := make([]key, 0, len(mem)+len(sysDisk)+len(poolDisk))

	addEntry := func(k key, entry PressuredNode) *PressuredNode {
		if existing, ok := index[k]; ok {
			return existing
		}
		index[k] = &entry
		order = append(order, k)
		return index[k]
	}

	for _, r := range mem {
		k := key{node: r.NodeEffectiveAvailability.Name}
		e := addEntry(k, PressuredNode{Node: r.NodeEffectiveAvailability.Name})
		e.Conditions = appendUnique(e.Conditions, "memory_pressure")
		if r.NodeEffectiveAvailability.MemoryPressureSince != nil && e.MemoryPressureSince == nil {
			t := r.NodeEffectiveAvailability.MemoryPressureSince.UTC()
			e.MemoryPressureSince = &t
		}
	}
	for _, r := range sysDisk {
		k := key{node: r.NodeEffectiveAvailability.Name}
		e := addEntry(k, PressuredNode{Node: r.NodeEffectiveAvailability.Name})
		e.Conditions = appendUnique(e.Conditions, "system_disk_pressure")
		if r.NodeEffectiveAvailability.SystemDiskPressureSince != nil && e.SystemDiskPressureSince == nil {
			t := r.NodeEffectiveAvailability.SystemDiskPressureSince.UTC()
			e.SystemDiskPressureSince = &t
		}
	}
	for _, r := range poolDisk {
		k := key{node: r.NodeEffectiveAvailability.Name, pool: r.PoolEffectiveCapacity.Name}
		e := addEntry(k, PressuredNode{
			Node: r.NodeEffectiveAvailability.Name,
			Pool: r.PoolEffectiveCapacity.Name,
		})
		e.Conditions = appendUnique(e.Conditions, "disk_pressure")
		if r.PoolEffectiveCapacity.DiskPressureSince != nil && e.DiskPressureSince == nil {
			t := r.PoolEffectiveCapacity.DiskPressureSince.UTC()
			e.DiskPressureSince = &t
		}
	}

	out := make([]PressuredNode, 0, len(order))
	for _, k := range order {
		out = append(out, *index[k])
	}
	return NodePressureDetail{Nodes: out}
}

func appendUnique(out []string, s string) []string {
	for _, existing := range out {
		if existing == s {
			return out
		}
	}
	return append(out, s)
}

// clampInt32 narrows the validated int request fields into the int32
// shape NodeUtilization expects. validateCreateRequest already caps
// vcpus to 128 and memory to 524288, so saturation never trips in
// production — the bound is for defensive consistency.
func clampInt32(v int) int32 {
	const maxInt32 = int32(1<<31 - 1)
	if v > int(maxInt32) {
		return maxInt32
	}
	if v < 0 {
		return 0
	}
	return int32(v) //nolint:gosec // bounded immediately above
}
