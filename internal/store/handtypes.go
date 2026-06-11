// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/queue"
)

// This file holds the hand-written domain types and narrow store
// interfaces the handlers depend on (as opposed to the per-query Params/Row
// types in the *_types.go files). The store implementation realises the
// interfaces and consumes/produces the structs.

// HeartbeatProjection is the narrow store surface the heartbeat reconcile
// projects through. The store runs the reconcile against an implementation of
// this interface so the projection logic stays in the handler, decoupled from
// the concrete store, and the handler need not import it. The reconcile is not
// one atomic transaction: each method applies its write directly, which is safe
// because the projection is idempotent and retried forever (the next heartbeat
// re-applies and converges).
type HeartbeatProjection interface {
	NodeForHeartbeat(ctx context.Context, nodeID uuid.UUID) (GetNodeForHeartbeatRow, error)
	NodeByID(ctx context.Context, id uuid.UUID) (Node, error)
	NodeByIDAtRev(ctx context.Context, id uuid.UUID, rev int64) (Node, error)
	UpdateNodeHeartbeat(ctx context.Context, arg UpdateNodeHeartbeatParams) error
	UpdateNodeMemoryPressure(ctx context.Context, arg UpdateNodeMemoryPressureParams) error
	UpdateNodeSystemDiskPressure(ctx context.Context, arg UpdateNodeSystemDiskPressureParams) error
	FilterExistingVMIDs(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error)
	// FilterVMIDsPinnedToNode returns the subset of ids whose vms row is pinned
	// to nodeID (PinnedNodeID set and equal). It is the placement-authority gate
	// for the heartbeat: a node may only report runtime for VMs the scheduler
	// pinned to it, so a heartbeat cannot move vm_runtime.current_node_id to a
	// node the VM is not placed on.
	FilterVMIDsPinnedToNode(ctx context.Context, nodeID uuid.UUID, ids []uuid.UUID) ([]uuid.UUID, error)
	UpsertVMRuntime(ctx context.Context, arg UpsertVMRuntimeParams) error
	UpdateStoragePoolReconciliation(ctx context.Context, arg UpdateStoragePoolReconciliationParams) error
	// StoragePoolIDByNodeName resolves a pool's UUID from its (node_id,
	// lower(name)) identity, the same join key UpdateStoragePoolReconciliation
	// matches on. found is false (nil error) when no live pool matches, so a
	// caller keying an observed-state write on the id can skip it safely rather
	// than persist an orphan keyed on a pool the CP no longer has.
	StoragePoolIDByNodeName(ctx context.Context, nodeID uuid.UUID, name string) (uuid.UUID, bool, error)
	// UpsertPoolImageInventory replaces the observed image inventory for a pool.
	// An empty slice clears it so a pool that dropped all images reports empty,
	// not stale.
	UpsertPoolImageInventory(ctx context.Context, poolID uuid.UUID, images []PoolImage) error
	ListStoragePoolsByNode(ctx context.Context, nodeID uuid.UUID) ([]StoragePool, error)
	UpsertNetworkNodeStatus(ctx context.Context, arg UpsertNetworkNodeStatusParams) error
	ListNetworks(ctx context.Context) ([]Network, error)
	// ListVMNicsByNetwork returns the non-deleted NIC rows attached to the
	// network, reconciling the per-network index against live rows. The
	// projection joins them into declared_networks[].reservations (MAC ->
	// Ipv4Address) for the agent's per-network DHCP.
	ListVMNicsByNetwork(ctx context.Context, networkID uuid.UUID) ([]VMNic, error)
	UpsertAgentWireguard(ctx context.Context, arg UpsertAgentWireguardParams) error
	ListAgentWireguard(ctx context.Context) ([]AgentWireguard, error)
	ListAgentWireguardAtRev(ctx context.Context, rev int64) ([]AgentWireguard, error)
	AgentWireguardByNodeID(ctx context.Context, nodeID uuid.UUID) (AgentWireguard, error)
	OverlaySupernet(ctx context.Context) (netip.Prefix, error)
	UnderlayMTU(ctx context.Context) (int32, error)
	ListVMsForNodeDeclared(ctx context.Context, nodeID uuid.UUID) ([]ListVMsForNodeDeclaredRow, error)
	ListOverlayNICPlacements(ctx context.Context) ([]OverlayNICPlacement, error)
	// ListOverlayNICPlacementsPinned returns the overlay NIC placements together
	// with the MVCC revision the read is pinned to (its first range's revision).
	// The FDB projection threads that revision into ListAgentWireguardAtRev and
	// NodeByIDAtRev so the whole join sees one consistent etcd snapshot.
	ListOverlayNICPlacementsPinned(ctx context.Context) ([]OverlayNICPlacement, int64, error)
}

// OverlayNICPlacement is one NIC attached to a type=overlay network whose owning
// VM has a current node. The heartbeat FDB projection joins these (MAC + owning
// node) to each node's VTEP overlay IP to build declared_fdb.
type OverlayNICPlacement struct {
	VNI    int32
	Mac    net.HardwareAddr
	NodeID uuid.UUID
}

// PlacementReader is the read surface SchedulePlacement consumes, plus the
// advisory-lock acquisition that serialises the placement decision window across
// replicas. A store backend implements it for the vm.create handler.
type PlacementReader interface {
	AcquirePlacementLock(ctx context.Context, lockKey int64) error
	ListEligiblePoolsByName(ctx context.Context, name string) ([]ListEligiblePoolsByNameRow, error)
	ListMemoryPressuredCandidatesByName(ctx context.Context, name string) ([]ListMemoryPressuredCandidatesByNameRow, error)
	ListSystemDiskPressuredCandidatesByName(ctx context.Context, name string) ([]ListSystemDiskPressuredCandidatesByNameRow, error)
	ListDiskPressuredPoolsByName(ctx context.Context, name string) ([]ListDiskPressuredPoolsByNameRow, error)
	ListStoragePoolsByName(ctx context.Context, name string) ([]StoragePool, error)
	CountRunningVMsByNode(ctx context.Context, nodeID *uuid.UUID) (int64, error)
	ListNetworkNodeStatusByNode(ctx context.Context, nodeID uuid.UUID) ([]NetworkNodeStatus, error)
	// NetworkByName resolves the deferred network name captured at admission to
	// its row, so the bind can set the NIC's network id. Returns ErrNotFound
	// when no live network owns the name (a pending scheduling reason).
	NetworkByName(ctx context.Context, name string) (Network, error)
}

// VMCreateWrites bundles the rows a vm.create commits atomically: the VM, its
// boot disk, an optional network interface, the task, and the enqueued job
// args. Nic is nil when the create request named no network (legacy SLIRP
// fallback on the agent); when set it lands in the same transaction as the VM
// so the network's delete-block index is consistent the instant the VM exists.
//
// This is the input to the direct-bind etcdstore.CreateScheduledVM path, now
// test-only; the production reconcile path uses CreateVMParams +
// VMBindWrites. See CreateScheduledVM.
type VMCreateWrites struct {
	VM   CreateVMParams
	Disk CreateVMDiskParams
	Nic  *CreateVMNicParams
	Task CreateTaskParams
	Job  queue.JobArgs
}

// RedeemJoinTokenParams carries the already-validated redemption inputs.
// Request-shape and CSR validation are the caller's responsibility; this struct
// holds only what the atomic redemption needs.
type RedeemJoinTokenParams struct {
	TokenHash               []byte
	NodeName                string
	Architecture            CPUArch
	AdvertisedEndpoint      string
	MigrationHost           string
	MigrationPortRangeStart int32
	MigrationPortRangeEnd   int32
	SourceIP                *netip.Addr
}

// UpsertAgentWireguardParams carries an agent's observed WG state for the
// heartbeat up-channel ingest. AgentIndex / OverlayIP are CP-assigned, not part
// of the agent's report, so they are absent here.
type UpsertAgentWireguardParams struct {
	NodeID               uuid.UUID
	PublicKey            string
	Endpoint             string
	ListenPort           int32
	EstablishedPeers     []string
	ReconciliationStatus string
	ReconciliationError  *string
}

// IssuedCert is the metadata the redemption persists for a freshly signed agent
// cert. The signing itself (x509 / crypto) lives in the caller's sign callback
// so the store stays driver-and-crypto-agnostic; only the resulting metadata
// crosses back in.
type IssuedCert struct {
	Serial            []byte
	FingerprintSha256 []byte
	SubjectDN         string
	NotBefore         time.Time
	NotAfter          time.Time
}

// RedeemJoinTokenResult reports the ids touched by a successful redemption, for
// the caller's audit logging after commit.
type RedeemJoinTokenResult struct {
	NodeID  uuid.UUID
	TokenID uuid.UUID
}

// RedeemClusterJoinTokenParams carries a cluster-replica join redemption: a
// joining control-plane replica presents a kind=cluster token to obtain the
// cluster CA. No CSR / node identity - the replica receives the CA cert + key
// and signs its own peer cert locally.
type RedeemClusterJoinTokenParams struct {
	TokenHash []byte
	SourceIP  *netip.Addr
}

// RedeemClusterJoinResult reports the redeemed cluster token id (for the
// consumption audit trail); the CA material itself is loaded by the handler
// from the active CA row.
type RedeemClusterJoinResult struct {
	TokenID uuid.UUID
}

// NodeDeleteOutcome reports the side effects of a force node delete: the VM
// runtimes orphaned and the migrations cancelled.
type NodeDeleteOutcome struct {
	VMsOrphaned         int64
	MigrationsCancelled int64
}
