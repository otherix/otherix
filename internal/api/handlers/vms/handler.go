// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package vms hosts the /v1/vms/* HTTP handlers - the operator-facing
// CP surface for vm.create / vm.get / vm.list / vm.delete.
//
// **Spec drift note:** api/openapi/control-plane.yaml declares a rich
// design (`VMCreate` / `VMSpec` shapes with multi-disk, network
// policy, cloud-init metadata, etc.). The current implementation
// ships the simplified `{name, image_url, image_sha256?, arch,
// firmware?, format?, disk_gib?, pool, vcpus, memory_mb}` shape - same
// drift policy as the agent.yaml vs the initial agent's hand-written
// wire shape. Reconciling the spec to the implementation is future
// work; the current path is documented in code (this comment +
// create.go's request shape) so operators reading either source land
// at the truth.
//
// Wire-shape translation: the API request carries the simplified
// image-source shape above; the schema stores `cpu_cores`,
// `memory_mib`, the image fields (`image_url`, `image_sha256`,
// `image_format`, `architecture`, `firmware_id`) on the vms row, and
// no top-level pool / node columns. The handler boundary owns the
// translation:
//
//   - vcpus    ↔ vms.cpu_cores
//   - memory_mb ↔ vms.memory_mib
//   - pool_id  ↔ vm_disks.storage_pool_id (pool tracking lives on
//     vm_disks, not vms)
//
// Lifecycle status is **projected**, not stored. The wire `status`
// string flows from (vms.deleted_at, vm_runtime.phase) per
// projection.go's truth table — no schema column carries the
// user-facing string. See projectStatus().
//
// RBAC composition:
//
//   - vm:create gate (RequirePermission middleware) admits admin /
//     operator / developer; viewer is 403 at the route. There is no
//     handler-side composite check: any image URL the caller supplies
//     is materialized by the agent on create (the former
//     template-usability check went away with the template entity).
//   - vm:read gate is held at scope=any by every authenticated role;
//     no ownership check on the inventory fields. ListVMsByOwner stays
//     inactive. The secret-bearing view fields (user_data,
//     network_config, image_url) are the carve-out: they are stripped
//     unless the caller holds vm:console on that VM (see
//     callerCanReadVMSecrets), closing the cross-owner cloud-init /
//     presigned-URL leak (audit R2-H1).
//   - vm:delete gate admits admin (any) / operator (any) / developer
//     (own); auth.CheckOwnership inside handler enforces own→404
//     bridge per CLAUDE.md "403 vs 404" rule.
package vms

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/handlers/internal/resolver"
	"github.com/otherix/otherix/internal/queue"
	"github.com/otherix/otherix/internal/store"
)

// Store is the storage surface the vms handlers depend on: the VM
// domain methods, the identifier-resolution contract (resolver.Querier)
// used to resolve vm / pool / node parameters, the node and firmware
// reads used by the view projectors and firmware resolution, the admission-only
// CreateUnscheduledVM seam, and the EnqueueTask producer
// seam used by delete and the async lifecycle ops. *etcdstore.Store
// satisfies it; depending on the interface rather than the concrete
// store narrows the handler's storage dependency to the methods it uses,
// lets tests substitute a fake, and keeps the queue off the
// request handlers. The vm create/delete/lifecycle workers (jobs.go)
// are consumer-side and hold the concrete store.
type Store interface {
	resolver.Querier

	VMByID(ctx context.Context, id uuid.UUID) (store.VM, error)
	VMRuntimeByID(ctx context.Context, vmID uuid.UUID) (store.VMRuntime, error)
	ListVMDisksByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMDisk, error)
	ListVMNicsByVM(ctx context.Context, vmID uuid.UUID) ([]store.VMNic, error)
	ListVMs(ctx context.Context, arg store.ListVMsParams) ([]store.VM, error)
	UpdateVMRuntimePhase(ctx context.Context, arg store.UpdateVMRuntimePhaseParams) error
	NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error)
	UserByID(ctx context.Context, id uuid.UUID) (store.User, error)
	FirmwareByID(ctx context.Context, id uuid.UUID) (store.Firmware, error)
	DefaultFirmwareForArchType(ctx context.Context, arch store.CPUArch, ftype store.FirmwareType) (store.Firmware, error)
	NetworkByID(ctx context.Context, id uuid.UUID) (store.Network, error)
	NetworkByName(ctx context.Context, name string) (store.Network, error)
	CreateUnscheduledVM(ctx context.Context, p store.CreateVMParams) (uuid.UUID, error)
	DeleteUnscheduledVM(ctx context.Context, vmID uuid.UUID) error
	StoragePoolsByName(ctx context.Context, name string) ([]store.StoragePool, error)
	EnqueueTask(ctx context.Context, params store.CreateTaskParams, args queue.JobArgs) (uuid.UUID, error)
	ActiveVMDeleteTaskVMIDs(ctx context.Context) (map[uuid.UUID]struct{}, error)
	// ActiveMigrationForVM returns the VM's in-flight (non-terminal)
	// migration: read by the VM status.migration summary and by the
	// stop/delete lifecycle-precedence path. CancelMigration ends that
	// migration (cancelled) before stop/delete enqueues - the desired
	// lifecycle outranks "I want it on another node" (spec D5).
	ActiveMigrationForVM(ctx context.Context, vmID uuid.UUID) (store.Migration, bool, error)
	CancelMigration(ctx context.Context, id uuid.UUID, reason string) (store.Migration, error)
}

// Ensure the production store satisfies the handler's storage contract.

// Handler bundles the dependencies for the vms routes. Create persists
// the VM via the store's admission-only CreateUnscheduledVM seam; Delete
// and the async lifecycle ops enqueue through EnqueueTask, so the handler
// no longer holds a queue client. Placement moved out of the handler: the
// vms.schedule reconcile loop (cmd/api wires ScheduleFunc) binds pending
// VMs, so the handler carries no placement config.
type Handler struct {
	store       Store
	log         *slog.Logger
	lifecycle   LifecycleDeps
	consoleDeps ConsoleDeps
}

// New constructs a Handler. It takes the Store interface so any
// conforming backend can be wired in; production passes *store.Store.
// lifecycle bundles the agentclient dependency the sync Pause / Resume /
// Reset handlers need; tests that exercise the sync surface pass a stub
// through here without importing the production client. console bundles the
// console-handler deps (agentclient + access mode); tests that don't
// exercise the console flow pass a zero-value ConsoleDeps.
func New(
	s Store,
	log *slog.Logger,
	lifecycle LifecycleDeps,
	console ConsoleDeps,
) *Handler {
	return &Handler{
		store:       s,
		log:         log,
		lifecycle:   lifecycle,
		consoleDeps: console,
	}
}

// vmView mirrors components/schemas/VM in api/openapi/control-plane.yaml.
// Referenced-resource fields are rendered as names instead of UUIDs:
//
//   - pool carries storage_pools.name (drawn from vm_disks[0].storage_pool_id),
//   - node carries the agent-reported current location (vm_runtime.current_node_id)
//     rendered as nodes.name; nil while the VM is still in 'creating'.
//
// The image source is surfaced directly (image_url + format); the VM row is
// self-describing, there is no template entity. status is projected (not
// stored) - see projection.go. owner_id always carries the UUID (stable,
// round-trippable); owner carries the owner's display_name (or email when
// unset) but only for callers holding user:read (admin / operator) - it
// stays null for
// developer / viewer so the VM surface cannot be used to enumerate the user
// directory those roles cannot otherwise read.
type vmView struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	OwnerID string  `json:"owner_id"`
	Owner   *string `json:"owner"`
	// ImageURL is secret-gated like UserData / NetworkConfig below: a
	// presigned private-mirror URL can embed credentials, so it is omitted
	// for callers without vm:console on this VM. ImageSHA256 and Format are
	// inventory (a digest and a format string reveal no secret) and stay
	// visible to every vm:read holder.
	ImageURL     string          `json:"image_url,omitempty"`
	ImageSHA256  string          `json:"image_sha256,omitempty"`
	Format       string          `json:"format"`
	Pool         string          `json:"pool"`
	Node         *string         `json:"node"`
	Networks     []string        `json:"networks"`
	Nics         []nicView       `json:"nics"`
	Architecture string          `json:"architecture"`
	VCPUs        int             `json:"vcpus"`
	MemoryMB     int             `json:"memory_mb"`
	Status       vmStatusView    `json:"status"`
	DesiredPhase string          `json:"desired_phase"`
	Labels       json.RawMessage `json:"labels"`
	// UserData and NetworkConfig echo the cloud-init payloads the VM was
	// created with, verbatim - but ONLY to callers holding vm:console on
	// this VM (owner for developer scope=own; any VM for admin / operator).
	// Those callers can already extract the payloads from inside the guest,
	// so the view reveals nothing new to them; everyone else (viewer, a
	// developer reading a foreign VM) gets the fields stripped, because
	// cloud-init routinely carries guest passwords, SSH keys, and API
	// tokens. See callerCanReadVMSecrets and the includeSecrets parameter
	// on toView (audit finding R2-H1).
	UserData      *string `json:"user_data,omitempty"`
	NetworkConfig *string `json:"network_config,omitempty"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
}

// nicView is the compact per-NIC projection on the public VM shape. Network is
// the resolved network name (same resolution as the parallel Networks slice);
// MAC is the NIC's hardware address; Ipv4Address carries the CP-IPAM-allocated
// host as a string, or nil when the NIC has no allocation (e.g. a plain bridge
// network, where IPAM does not run).
type nicView struct {
	Network     string  `json:"network"`
	MAC         string  `json:"mac_address"`
	Ipv4Address *string `json:"ipv4_address"`
}

// vmStatusView is the nested `status` object on the public VM shape.
// Phase is the projected lifecycle string (see projectStatus); Reason
// and Message carry the machine-readable / human-readable scheduling
// detail and are populated only while the VM is unscheduled (pending),
// omitted otherwise.
type vmStatusView struct {
	Phase   string `json:"phase"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	// Migration is the compact summary of an in-flight migration for this
	// VM (Observability surface 3): an operator inspecting the VM sees
	// "migrating -> target, 47%" without first knowing the migration id.
	// Omitted when no migration is active. current_node_id (rendered as
	// the top-level `node` field) stays = source until cutover, so the
	// summary's target_node_id is the only forward-looking signal here.
	Migration *vmMigrationSummary `json:"migration,omitempty"`
}

// vmMigrationSummary is the compact in-flight-migration view embedded under
// status.migration. It mirrors the four fields spec Observability surface 3
// names (id, phase, progress_percent, target_node) - the full record lives at
// GET /v1/migrations/{id}. target_node_id is nil for a node-less (scheduler-
// placed) migration that has not bound a target yet.
type vmMigrationSummary struct {
	ID              string  `json:"id"`
	Phase           string  `json:"phase"`
	ProgressPercent int     `json:"progress_percent"`
	TargetNodeID    *string `json:"target_node_id"`
}

// migrationSummary projects an active store.Migration onto its compact
// status.migration view, or returns nil when m is nil.
func migrationSummary(m *store.Migration) *vmMigrationSummary {
	if m == nil {
		return nil
	}
	var target *string
	if m.TargetNodeID != nil {
		s := m.TargetNodeID.String()
		target = &s
	}
	return &vmMigrationSummary{
		ID:              m.ID.String(),
		Phase:           string(m.Phase),
		ProgressPercent: int(m.ProgressPercent),
		TargetNodeID:    target,
	}
}

// vmViewNames bundles the resolved name lookups that response
// rendering requires. Each field may be empty / nil when the underlying
// row is missing or the FK was nulled out. The caller pre-resolves;
// toView only renders.
type vmViewNames struct {
	pool string
	node *string
	// owner is the owner's display_name (or email when unset), resolved
	// only when the caller holds user:read; nil otherwise (or when the
	// owner row was deleted).
	owner *string
	// networks holds the VM's attached network names ordered by NIC
	// device_order (primary first). Empty when the VM has no NIC.
	networks []string
	// nics holds the compact per-NIC views (network name, MAC, allocated
	// IPv4) in the same NIC device_order as networks, so networks[i] and
	// nics[i] line up. Empty when the VM has no NIC. Derived from the same
	// ListVMNicsByVM load that produces networks - no extra store round-trip.
	nics []nicView
}

// toView projects a (vm row, runtime row) pair plus the pre-resolved
// names onto its public vmView. runtime is nil when no vm_runtime row
// exists yet (worker has not upserted) — projectStatus collapses that
// case to "creating". The vm_disks row is consumed upstream to derive
// names.pool and is not threaded through here. deleting is true when a
// non-terminal vm.delete task targets this VM, which projectStatus
// surfaces as status.phase "deleting".
//
// While the VM is unscheduled (pending) it has no disk / NIC rows yet,
// so names.pool / names.networks are empty; toView then falls back to
// the SchedulingSpec so the operator still sees the requested pool and
// network. The scheduling reason / message surface on status.
//
// includeSecrets gates the secret-bearing fields (user_data,
// network_config, image_url): when false they are left at their zero
// values so the omitempty wire fields render absent. Callers compute it
// per-VM via callerCanReadVMSecrets(ctx, vm.OwnerID) - vm:console on
// this VM is the access signal (audit R2-H1).
//
// activeMig is the VM's in-flight (non-terminal) migration, or nil. When
// non-nil it populates status.migration (Observability surface 3); it never
// changes names.node - current_node_id stays = source until cutover.
func toView(vm store.VM, runtime *store.VMRuntime, names vmViewNames, deleting, includeSecrets bool, activeMig *store.Migration) vmView {
	status := vmStatusView{Phase: projectStatus(vm, runtime, deleting, activeMig != nil), Migration: migrationSummary(activeMig)}
	pool := names.pool
	nets := names.networks
	if vm.SchedulingStatus == store.VMSchedulingUnscheduled {
		if vm.SchedulingReason != nil {
			status.Reason = *vm.SchedulingReason
		}
		if vm.SchedulingMessage != nil {
			status.Message = *vm.SchedulingMessage
		}
		if sp, err := store.UnmarshalSchedulingSpec(vm.SchedulingSpec); err == nil {
			if pool == "" {
				pool = sp.PoolName
			}
			if len(nets) == 0 && sp.NetworkName != nil {
				nets = []string{*sp.NetworkName}
			}
		}
	}
	v := vmView{
		ID:            vm.ID.String(),
		Name:          vm.Name,
		OwnerID:       vm.OwnerID.String(),
		Owner:         names.owner,
		ImageURL:      vm.ImageURL,
		ImageSHA256:   hex.EncodeToString(vm.ImageSHA256),
		Format:        string(vm.ImageFormat),
		Pool:          pool,
		Node:          names.node,
		Networks:      networksOrEmpty(nets),
		Nics:          nicsOrEmpty(names.nics),
		Architecture:  string(vm.Architecture),
		VCPUs:         int(vm.CpuCores),
		MemoryMB:      int(vm.MemoryMib),
		Status:        status,
		DesiredPhase:  string(vm.DesiredPhase),
		Labels:        rawJSONOrEmpty(vm.Labels),
		UserData:      vm.UserData,
		NetworkConfig: vm.NetworkConfig,
		CreatedAt:     vm.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:     vm.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if !includeSecrets {
		v.ImageURL = ""
		v.UserData = nil
		v.NetworkConfig = nil
	}
	return v
}

// networksOrEmpty normalises a nil network slice to a non-nil empty
// slice so the wire `networks` field renders `[]` (the schema declares
// it a required array) rather than `null` for a VM with no NIC.
func networksOrEmpty(nets []string) []string {
	if nets == nil {
		return []string{}
	}
	return nets
}

// nicsOrEmpty normalises a nil NIC slice to a non-nil empty slice so the wire
// `nics` field renders `[]` (a required array) rather than `null` for a VM with
// no NIC.
func nicsOrEmpty(nics []nicView) []nicView {
	if nics == nil {
		return []nicView{}
	}
	return nics
}

// rawJSONOrEmpty returns raw if non-empty, else `{}`. vms.labels is
// NOT NULL with a `'{}'` default; the helper guards the wire format
// in unusual restore / rollback paths where the bytes might be empty.
func rawJSONOrEmpty(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return json.RawMessage(raw)
}
