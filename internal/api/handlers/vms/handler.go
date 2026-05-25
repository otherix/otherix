// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package vms hosts the /v1/vms/* HTTP handlers - the operator-facing
// CP surface for vm.create / vm.get / vm.list / vm.delete.
//
// **Spec drift note:** api/openapi/control-plane.yaml declares a rich
// design (`VMCreate` / `VMSpec` shapes with multi-disk, network
// policy, cloud-init metadata, etc.). The current implementation
// ships the simplified `{name, template_id, pool_id, vcpus,
// memory_mb}` shape - same drift policy as the agent.yaml vs the
// initial agent's hand-written wire shape. Reconciling the spec to
// the implementation is future work; the current path is documented
// in code (this comment + create.go's request shape) so operators
// reading either source land at the truth.
//
// Wire-shape translation: the API request carries the simplified
// `{name, template_id, pool_id, vcpus, memory_mb}` shape; the schema
// stores `cpu_cores`, `memory_mib`, no top-level pool / node columns.
// The handler boundary owns the translation:
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
//     operator / developer; viewer is 403 at the route. Handler-side
//     composite check is template-usability, mirroring
//     templates.image_import.checkImportAccess: ScopeAny → ok;
//     ScopeOwn + caller==template.owner → ok; public template +
//     template:read:public → ok; else 404 (no existence leak).
//   - vm:read gate is held at scope=any by every authenticated role;
//     no ownership check. ListVMsByOwner stays inactive.
//   - vm:delete gate admits admin (any) / operator (any) / developer
//     (own); auth.CheckOwnership inside handler enforces own→404
//     bridge per CLAUDE.md "403 vs 404" rule.
package vms

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/scheduler"
	"github.com/otherix/otherix/internal/store"
)

// Handler bundles the dependencies for the vms routes. The CRUD
// surface needs the store; Create / Delete additionally need the river
// client to enqueue the underlying job inside the same transaction as
// the task row insert. placementAlgorithm threads through to
// internal/scheduler.SchedulePlacement; the empty string defers к the
// package default ("resource_aware"). placementResources threads the
// per-resource gate - zero-value disables every resource и degrades
// scoring к count-based fallback,
// so production wiring always passes the config.ResourcesConfig the
// api binary validated at startup.
type Handler struct {
	store              *store.Store
	riverClient        *river.Client[pgx.Tx]
	log                *slog.Logger
	placementAlgorithm string
	placementResources scheduler.ResourcesConfig
	lifecycle          LifecycleDeps
	consoleDeps        ConsoleDeps
}

// New constructs a Handler. riverClient is required for Create /
// Delete; the production wiring (cmd/api/main.go) always supplies one.
// placementAlgorithm is the validated APIConfig.Placement.Algorithm
// value — pass "" к accept the scheduler default. placementResources
// pins the per-resource gate; pass а zero-value (every resource
// disabled) only in tests / scaffolding contexts that explicitly want
// count-based scoring across the board. lifecycle bundles the
// agentclient dependency the sync Pause / Resume / Reset handlers
// need; tests that exercise the sync surface pass а stub through
// here без importing the production client. console bundles the
// console-handler deps (agentclient + access mode); tests that don't
// exercise the console flow pass а zero-value ConsoleDeps.
func New(
	s *store.Store,
	riverClient *river.Client[pgx.Tx],
	log *slog.Logger,
	placementAlgorithm string,
	placementResources scheduler.ResourcesConfig,
	lifecycle LifecycleDeps,
	console ConsoleDeps,
) *Handler {
	return &Handler{
		store:              s,
		riverClient:        riverClient,
		log:                log,
		placementAlgorithm: placementAlgorithm,
		placementResources: placementResources,
		lifecycle:          lifecycle,
		consoleDeps:        console,
	}
}

// vmView mirrors components/schemas/VM in api/openapi/control-plane.yaml.
// Referenced-resource fields are rendered as names instead of UUIDs:
//
//   - template carries templates.name (nil when the source template has
//     been deleted — vms.template_id is ON DELETE SET NULL),
//   - pool carries storage_pools.name (drawn from vm_disks[0].storage_pool_id),
//   - node carries the agent-reported current location (vm_runtime.current_node_id)
//     rendered as nodes.name; nil while the VM is still in 'creating'.
//
// status is projected (not stored) - see projection.go. owner_id keeps
// the UUID rendering: users are outside the resolver scope, so
// narrowing to a username would lose round-trippability.
type vmView struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	OwnerID      string          `json:"owner_id"`
	Template     *string         `json:"template"`
	Pool         string          `json:"pool"`
	Node         *string         `json:"node"`
	Architecture string          `json:"architecture"`
	VCPUs        int             `json:"vcpus"`
	MemoryMB     int             `json:"memory_mb"`
	Status       string          `json:"status"`
	DesiredPhase string          `json:"desired_phase"`
	Labels       json.RawMessage `json:"labels"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
}

// vmViewNames bundles the resolved name lookups that response
// rendering requires. Each field may be empty / nil when the underlying
// row is missing or the FK was nulled out (template deletion). The
// caller pre-resolves; toView only renders.
type vmViewNames struct {
	template *string
	pool     string
	node     *string
}

// toView projects a (vm row, runtime row) pair plus the pre-resolved
// names onto its public vmView. runtime is nil when no vm_runtime row
// exists yet (worker has not upserted) — projectStatus collapses that
// case to "creating". The vm_disks row is consumed upstream to derive
// names.pool and is not threaded through here.
func toView(vm store.VM, runtime *store.VMRuntime, names vmViewNames) vmView {
	return vmView{
		ID:           vm.ID.String(),
		Name:         vm.Name,
		OwnerID:      vm.OwnerID.String(),
		Template:     names.template,
		Pool:         names.pool,
		Node:         names.node,
		Architecture: string(vm.Architecture),
		VCPUs:        int(vm.CpuCores),
		MemoryMB:     int(vm.MemoryMib),
		Status:       projectStatus(vm, runtime),
		DesiredPhase: string(vm.DesiredPhase),
		Labels:       rawJSONOrEmpty(vm.Labels),
		CreatedAt:    vm.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt:    vm.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
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
