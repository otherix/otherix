// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package vm hosts the agent's VM lifecycle orchestrator: Manager
// drives create / delete / get / list flows and exposes per-task
// status through an in-memory TaskStore. State persists to disk via
// internal/agent/state (meta.json per VM); runtime supervision and
// QMP control go through internal/agent/qemu.
//
// Task IDs are agent-local — they share no namespace with CP-side
// task IDs from internal/store. CP integration (Iteration 3) maps
// CP river-job IDs onto agent task IDs through the
// agent_task_id resumption pattern documented in CLAUDE.md.
package vm

import (
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/qemu"
)

// Status enumerates the lifecycle states of an agent-managed VM. The
// The agent currently omits start/stop/pause/resume - running ↔ stopped
// transitions arrive in a later iteration. `paused` lands ahead of the rest
// of the runtime-state surface because Pre-L1 wiring needs it before
// L1 introduces pause/resume handlers.
type Status string

// Status values reachable in the Iteration 1 lifecycle.
const (
	StatusPending  Status = "pending"
	StatusCreating Status = "creating"
	StatusRunning  Status = "running"
	StatusPaused   Status = "paused"
	StatusStopping Status = "stopping"
	StatusStopped  Status = "stopped"
	StatusFailed   Status = "failed"
	StatusDeleting Status = "deleting"
	// StatusMigratingIncoming is the transitional status of a live-migration
	// target after the disk+RAM are adopted but before the resume driver
	// flips it to running. The VM reconciler and the status probe both leave
	// it alone (it falls in their default skip arms), so the adopted target
	// is never cold-started post-cutover. Resume transitions it to
	// StatusRunning.
	StatusMigratingIncoming Status = "migrating_incoming"
)

// VM is the agent's in-memory view of a managed virtual machine. The
// internal-only path fields are populated when Manager constructs the
// VM (either from a fresh CreateSpec or by replaying a meta.json on
// startup) and never crossed wire boundaries.
//
// The pool reference is a name. The agent's pool registry is
// name-keyed; the CP-side per-instance UUID stays at the operator
// API edge.
type VM struct {
	ID            uuid.UUID
	Name          string
	VCPUs         int
	MemoryMB      int
	PoolName      string
	Architecture  qemu.Architecture
	Status        Status
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DiskPath      string
	QMPSocket     string
	ConsoleSocket string
	PIDFile       string
	// CidataPath, if non-empty, points to the NoCloud cidata ISO
	// the agent generates during Create when the spec carries
	// UserData. Lives inside the per-VM state directory so the
	// existing teardown (RemoveAll of stateDir/<vmID>) cleans it.
	// Empty for VMs created without cloud-init (legacy /v1/vms calls
	// that omit the field; smoke tests that drop the seed).
	CidataPath string
	// NICs are the CP-declared network interfaces the manager
	// materialises (host tap + bridge) and wires into the qemu command
	// line. Empty means legacy SLIRP user-mode networking.
	NICs []netfabric.NIC
	// Migrated marks a VM whose disk was copied in from another node by a
	// migration. The create flow must NOT re-clone it from the base image
	// on first start; the disk is already the authoritative copy.
	Migrated bool
}

// CreateSpec is the wire-shape of a POST /v1/vms body, post-validation.
// Architecture is derived from the agent's host arch (currently supports
// same-arch only).
//
// UUID is optional: when uuid.Nil the manager generates a fresh id
// (backward-compatible with curl-driven smoke tests). When non-zero it is
// used as the VM id, which is how the CP-side worker passes the
// pre-minted vms.id through to the agent (unified UUID model: CP
// mints, agent uses).
type CreateSpec struct {
	UUID     uuid.UUID
	Name     string
	VCPUs    int
	MemoryMB int
	PoolName string
	// ImageURL is the HTTPS source the agent ensures locally (basename-keyed
	// IfNotPresent cache). ExpectedSHA256, when non-empty, pins the content
	// digest (verify mode). Format is "qcow2".
	ImageURL       string
	ExpectedSHA256 string
	Format         string
	// DiskGiB is the requested root-disk size. 0 means default to the image's
	// virtual size; a value below the virtual size is rejected; a larger value
	// grows the disk via qemu-img resize after the copy.
	DiskGiB int
	// UserData carries already-resolved raw `#cloud-config` YAML
	// per L3 Area 3 lock — CP-side has resolved vm.user_data and
	// injected hostname (there is no template fallback) when
	// needed. When empty (and NetworkData is also empty) the agent
	// skips cidata generation (legacy VMs still boot, just without a
	// NoCloud seed).
	UserData []byte
	// NetworkData carries the CP-supplied cloud-init network-config
	// (NoCloud /network-config; netplan v2 / net-config v1/v2),
	// passed through verbatim. Empty means cloud-init falls back to
	// DHCP discovery. Independent of UserData - either or both may be set.
	NetworkData []byte
	// CloudInitDisabled is the explicit opt-out (operator --no-cloud-init).
	// When true the agent skips the NoCloud seed entirely; otherwise every
	// create gets at least the minimal name-as-hostname seed (always-on
	// cidata). The CP persists this on the VM row and forwards it so empty
	// user-data no longer implies "no seed".
	CloudInitDisabled bool
	// SourceSnapshot, when non-nil, recreates the VM's disks from a snapshot's
	// content-addressed blobs instead of downloading an image. Mutually
	// exclusive with ImageURL: the CP vm.create worker resolves the snapshot
	// manifest (ordered disk digests) and sets exactly one of the two. The
	// agent clones each referenced blob from {poolRoot}/snapshots/{sha}.qcow2
	// in disk-index order; ImageURL is empty in this case.
	SourceSnapshot *SnapshotRef
	// NICs are the CP-declared network interfaces to materialise. Empty
	// means legacy SLIRP user-mode networking (curl-driven smoke tests
	// and pre-networking callers that omit the field).
	NICs []netfabric.NIC
}

// SnapshotRef is the resolved disk source for a recreate-from-snapshot create.
// Pool names the pool whose snapshots/ subdir holds the blobs (slice A K=1: the
// VM's own create pool); Disks lists every VM disk to materialize, ordered by
// virtio index, each carrying the content-addressed blob digest the agent
// clones. The CP worker resolves these from the snapshot manifest so the agent
// never needs CP store access.
type SnapshotRef struct {
	Pool  string
	Disks []SnapshotDiskRef
}

// SnapshotDiskRef is one disk in a SnapshotRef: its virtio index, the wire
// device name (virtio<i>), and the sha256 of the content-addressed blob under
// {poolRoot}/snapshots/{sha}.qcow2. Ordering by Index is the virtio<i>
// invariant the migration data-path and the Task-10 manifest also rely on.
type SnapshotDiskRef struct {
	Index  int
	Device string
	SHA256 string
}
