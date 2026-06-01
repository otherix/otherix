// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"
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
	UpdateNodeHeartbeat(ctx context.Context, arg UpdateNodeHeartbeatParams) error
	UpdateNodeMemoryPressure(ctx context.Context, arg UpdateNodeMemoryPressureParams) error
	UpdateNodeSystemDiskPressure(ctx context.Context, arg UpdateNodeSystemDiskPressureParams) error
	LookupFirmwareByCatalog(ctx context.Context, arg LookupFirmwareByCatalogParams) (uuid.UUID, error)
	UpsertNodeFirmware(ctx context.Context, arg UpsertNodeFirmwareParams) error
	FilterExistingVMIDs(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error)
	UpsertVMRuntime(ctx context.Context, arg UpsertVMRuntimeParams) error
	UpdateStoragePoolReconciliation(ctx context.Context, arg UpdateStoragePoolReconciliationParams) error
	ListStoragePoolsByNode(ctx context.Context, nodeID uuid.UUID) ([]StoragePool, error)
	UpsertNetworkNodeStatus(ctx context.Context, arg UpsertNetworkNodeStatusParams) error
	ListNetworks(ctx context.Context) ([]Network, error)
	ListVMsForNodeDeclared(ctx context.Context, nodeID uuid.UUID) ([]ListVMsForNodeDeclaredRow, error)
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
}

// VMCreateWrites bundles the four rows a vm.create commits atomically: the VM,
// its boot disk, the task, and the enqueued job args.
type VMCreateWrites struct {
	VM   CreateVMParams
	Disk CreateVMDiskParams
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
