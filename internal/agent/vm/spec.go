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

	"github.com/otherix/otherix/internal/agent/qemu"
)

// Status enumerates the lifecycle states of an agent-managed VM. The
// MVP omits start/stop/pause/resume - running ↔ stopped transitions
// arrive in a post-MVP iteration. `paused` lands ahead of the rest
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
}

// CreateSpec is the wire-shape of a POST /v1/vms body, post-validation.
// Architecture is derived from the agent's host arch (templates carry
// exactly one arch, MVP supports same-arch only).
//
// UUID is optional: when uuid.Nil the manager generates a fresh id
// (backward-compatible with curl-driven smoke tests). When non-zero it is
// used as the VM id, which is how the CP-side worker passes the
// pre-minted vms.id through to the agent (unified UUID model: CP
// mints, agent uses).
type CreateSpec struct {
	UUID             uuid.UUID
	Name             string
	VCPUs            int
	MemoryMB         int
	PoolName         string
	TemplateChecksum string
	// UserData carries already-resolved raw `#cloud-config` YAML
	// per L3 Area 3 lock — CP-side has merged vm.user_data ?:
	// template.cloud_init_user_data and injected hostname when
	// needed. When empty the agent skips cidata generation (legacy
	// VMs still boot, just without a NoCloud seed).
	UserData []byte
}
