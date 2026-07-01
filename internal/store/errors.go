// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"errors"
	"fmt"
)

// ErrNotFound is returned by a store backend when the requested row does not
// exist. Match it with errors.Is.
var ErrNotFound = errors.New("store: not found")

// ErrCACertActiveExists is returned by a store backend's CreateCACert when an
// active cluster CA row already exists, so the CA bootstrap can trap the
// lost-race path uniformly. Match it with errors.Is.
var ErrCACertActiveExists = errors.New("store: active CA cert already exists")

// Domain uniqueness / state sentinels surfaced by the store backends. Each maps
// to a uniqueness or precondition guard the backend enforces; handlers match
// them with errors.Is to produce the 409 conflict envelopes.
var (
	ErrUserEmailExists    = errors.New("store: user email already in use")
	ErrUserUsernameExists = errors.New("store: user username already in use")
	ErrNetworkNameExists  = errors.New("store: network name already in use")
	// ErrLoadBalancerNameExists reports a load-balancer name collision (the
	// cluster-wide case-insensitive name guard rejected the create/rename).
	ErrLoadBalancerNameExists = errors.New("store: load balancer name already in use")
	// ErrArtifactPoolNameExists is returned by CreateArtifactPool when a live
	// artifact pool already owns the (case-insensitive) name.
	ErrArtifactPoolNameExists = errors.New("store: artifact pool name already in use")
	// ErrPoolNameConflict is returned when a pool name collides across the
	// artifact-pool and storage-pool namespaces (a name denotes exactly one kind).
	ErrPoolNameConflict      = errors.New("store: pool name already used by a pool of the other kind")
	ErrNodeNameExists        = errors.New("store: node name already in use")
	ErrStoragePoolNameExists = errors.New("store: storage pool name already in use on node")
	ErrTaskNotCancellable    = errors.New("store: task not cancellable")
	ErrVMNameInUse           = errors.New("store: vm name already in use")
	ErrVMNicMACConflict      = errors.New("store: vm nic mac already in use on network")
	// ErrVMNotUnscheduled is returned by BindScheduledVM / UpdateVMSchedulingReason
	// when the VM is no longer in the "unscheduled" state (already bound, or
	// deleted) - the CAS lost. Callers (the scheduler loop) skip the VM.
	ErrVMNotUnscheduled = errors.New("store: vm is not unscheduled")
	// ErrRefreshTokenConflict is returned by RotateRefreshToken when the parent
	// token was already revoked or was rotated concurrently - the presented
	// token was double-spent. The auth service treats it as theft.
	ErrRefreshTokenConflict  = errors.New("store: refresh token rotated concurrently")
	ErrFirmwareNameExists    = errors.New("store: firmware name already in use for architecture")
	ErrFirmwareDefaultExists = errors.New("store: default firmware already exists for architecture and type")
	ErrJoinTokenInvalid      = errors.New("store: join token unknown or expired")
	ErrJoinTokenExhausted    = errors.New("store: join token max_uses exceeded")
	ErrJoinNodeNameMismatch  = errors.New("store: node name does not match token binding")
	ErrJoinNodeNameTaken     = errors.New("store: node already has an active cert")
	// ErrJoinNodeKindMismatch is returned when a redemption would reuse an
	// existing node row whose kind differs from the kind the token enrolls: a
	// node's kind is fixed by the token that first claims the name, so a later
	// redemption for a different kind must not silently reuse (and re-stamp) the
	// row. Surfaces as a 409 conflict.
	ErrJoinNodeKindMismatch = errors.New("store: node kind does not match token kind")

	// ErrMigrationActiveExists is returned by CreateMigration when the VM already
	// has a non-terminal migration: the per-VM active guard key is present, so a
	// second concurrent migration for the same VM loses the create CAS.
	ErrMigrationActiveExists = errors.New("active migration already exists for vm")

	// ErrConcurrentUpdate is returned by a CAS-guarded store update (e.g.
	// CommitMigrationCutover, UpdateMigrationProgress) when the migration or VM
	// row changed between the read and the commit: the ModRevision compare lost.
	// The caller re-reads and retries (the worker reconciles).
	ErrConcurrentUpdate = errors.New("store: row changed concurrently")

	// ErrMigrationTerminal is returned by an update path when the migration is
	// already in a terminal phase (completed/failed/cancelled) and cannot be
	// advanced further.
	ErrMigrationTerminal = errors.New("store: migration is terminal")

	// ErrMigrationNotCancelable is returned by CancelMigration when the migration
	// is already in a terminal phase (completed/failed/cancelled): cancel is valid
	// only pre-cutover, so a terminal migration cannot be cancelled.
	ErrMigrationNotCancelable = errors.New("store: migration is not cancelable")

	// ErrMigrationTargetConflict is returned by BindMigrationTarget when the
	// migration already has a target node that differs from the one being bound:
	// a node-less migration binds its scheduler-picked target exactly once, and a
	// second writer must not re-point it. Re-binding the same target is a no-op.
	ErrMigrationTargetConflict = errors.New("store: migration already bound to a different target")

	// ErrSnapshotNameExists is returned by CreateSnapshot when the VM already has
	// a non-deleted snapshot of the same name: the per-VM name guard key is
	// present, so a second create for the same (vm, name) loses the create CAS.
	ErrSnapshotNameExists = errors.New("store: snapshot name already exists for this vm")

	// ErrSnapshotHasChildren is returned by DeleteSnapshot when the snapshot still
	// has a non-deleted child (another snapshot whose ParentSnapshotID points at
	// it): delete is fail-closed and refuses until the children are removed first.
	ErrSnapshotHasChildren = errors.New("store: snapshot has non-deleted children")

	// ErrSnapshotSourcingCreate is returned by DeleteSnapshot when an active
	// (pending or running) vm.create task names this snapshot as its source: such
	// a create pulls the snapshot's blobs to the target node while it runs, so
	// deleting the source (which enqueues blob GC) could remove a blob the
	// in-flight create still needs. Delete is fail-closed and refuses until the
	// sourcing create reaches a terminal state.
	ErrSnapshotSourcingCreate = errors.New("store: snapshot is being sourced by an active vm create")

	// ErrNodeNotDrainable is returned when a drain is requested on a node whose
	// status is not ready or cordoned.
	ErrNodeNotDrainable = errors.New("store: node not drainable")

	// ErrIngressGrantNameExists is returned by CreateIngressGrant when a live grant
	// already owns the (case-insensitive) name: the name guard key is present,
	// so a second create for the same name loses the create CAS.
	ErrIngressGrantNameExists = errors.New("store: ingress grant name already in use")

	ErrAgentWireguardPubkeyInUse = errors.New("store: wireguard public key already in use by another node")
	ErrOverlaySupernetExhausted  = errors.New("store: overlay supernet has no free host address for a new agent")
	ErrVNIExhausted              = errors.New("store: overlay VNI range exhausted")
	ErrSubnetExhausted           = errors.New("store: network subnet exhausted")

	// ErrNetworkNotGatewayEligible is returned by CreateGatewayMembership when the
	// target network cannot back a gateway: only an overlay network with an
	// allocated VNI and a subnet carries the VNI and address space a membership
	// needs.
	ErrNetworkNotGatewayEligible = errors.New("store: network is not gateway-eligible")

	// ErrGatewayMembershipExists is returned by CreateGatewayMembership when the
	// gateway already covers the network: the (gateway_id, network_id) row is
	// present, so a second create loses the create CAS.
	ErrGatewayMembershipExists = errors.New("store: gateway already a member of network")
)

// ResourceInUseError reports that a resource cannot be deleted because other
// rows still reference it. Resources maps each blocking kind (e.g. "vms",
// "vm_disks") to its count and is non-empty. Handlers project it onto the API
// blocking_resources envelope. Shared across every resource whose delete is
// gated on dependants so the carrier stays uniform.
type ResourceInUseError struct {
	Resources map[string]int64
}

func (e *ResourceInUseError) Error() string {
	return fmt.Sprintf("resource in use: %v", e.Resources)
}

// UnderlayBelowFloorError reports that the cluster underlay MTU is below the
// floor needed to derive a valid (>= 1280) overlay MTU, so an overlay network
// cannot be created until the operator renumbers the underlay.
type UnderlayBelowFloorError struct {
	UnderlayMTU       int32
	MinUnderlayMTU    int32
	DerivedOverlayMTU int32
}

func (e *UnderlayBelowFloorError) Error() string {
	return fmt.Sprintf("underlay mtu %d is below the floor %d (derived overlay mtu %d < 1280)",
		e.UnderlayMTU, e.MinUnderlayMTU, e.DerivedOverlayMTU)
}
