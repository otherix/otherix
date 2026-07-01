// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

// Permission is a stable, machine-readable identifier of an action that
// the RBAC layer gates. Values use snake_case and a colon-separated
// hierarchy (`<resource>:<action>`); see docs/rbac.md for the full
// catalog and rationale.
//
// The catalog below is the runtime source of truth and must be kept in
// sync with docs/rbac.md by hand. Tests in permissions_test.go assert
// the structural invariants (admin holds all, viewer is read-only,
// developer's ownable resources are own-scoped, etc.).
type Permission string

// Permission constants. Group order mirrors docs/rbac.md.
const (
	// VM lifecycle. `vm:lifecycle` covers the desired-phase transitions
	// (start/stop/poweroff/reboot/reset/pause/resume); `vm:resize`
	// covers cpu/memory/disk capacity changes. They are intentionally
	// distinct from the catch-all `vm:update`.
	PermVMRead      Permission = "vm:read"
	PermVMCreate    Permission = "vm:create"
	PermVMUpdate    Permission = "vm:update"
	PermVMDelete    Permission = "vm:delete"
	PermVMLifecycle Permission = "vm:lifecycle"
	PermVMResize    Permission = "vm:resize"
	PermVMConsole   Permission = "vm:console"
	PermVMRevert    Permission = "vm:revert"
	PermVMMigrate   Permission = "vm:migrate"

	// SSH ingress. `vm:ssh` is the capability a grant token resolves to
	// at connect time (the synthetic GrantPrincipal carries exactly this
	// one permission, scoped to the grant's VM set). `vm:ingress-grant` gates
	// the operator-facing grant CRUD surface (/v1/ingress-grants). Both follow
	// the vm:console own/any shape: admin/operator any, developer own,
	// viewer none.
	PermVMSSH          Permission = "vm:ssh"
	PermVMIngressGrant Permission = "vm:ingress-grant"

	// PermVMConnect gates the control-plane connect-broker surface that
	// proxies an inbound session to a VM's ingress gateway. It follows the
	// vm:console own/any shape: admin/operator any, developer own, viewer
	// none.
	PermVMConnect Permission = "vm:connect"

	// Load balancers follow the vm:* own/any shape. read is any for every
	// role (names and metadata are not secret); create is the boolean
	// gate; update/delete/connect are own for developer, any for
	// admin/operator, none for viewer. `loadbalancer:connect` gates the
	// LB connect-broker and follows the vm-broker cross-owner-404
	// discipline.
	PermLoadBalancerRead    Permission = "loadbalancer:read"
	PermLoadBalancerCreate  Permission = "loadbalancer:create"
	PermLoadBalancerUpdate  Permission = "loadbalancer:update"
	PermLoadBalancerDelete  Permission = "loadbalancer:delete"
	PermLoadBalancerConnect Permission = "loadbalancer:connect"

	// Snapshots. Scoped against snapshots.owner_id (which today aligns
	// with the parent VM's owner; see docs/rbac.md edge-case note about
	// future ownership transfer).
	PermSnapshotRead   Permission = "snapshot:read"
	PermSnapshotCreate Permission = "snapshot:create"
	PermSnapshotDelete Permission = "snapshot:delete"
	PermSnapshotRevert Permission = "snapshot:revert"

	// Networks. No owner; only one scope makes sense.
	PermNetworkRead   Permission = "network:read"
	PermNetworkManage Permission = "network:manage"

	// Storage pools. `scan` is operator+admin; `manage` is admin-only
	// (pool lifecycle is host-coupled).
	PermStoragePoolRead   Permission = "storage_pool:read"
	PermStoragePoolManage Permission = "storage_pool:manage"
	PermStoragePoolScan   Permission = "storage_pool:scan"

	// Firmwares. There is no per-image authorization for VM creation
	// (vm:create is the single gate — any image URL the caller supplies is
	// fetched by the agent into the node-owned cache).
	PermFirmwareRead   Permission = "firmware:read"
	PermFirmwareManage Permission = "firmware:manage"

	// Nodes. The (full) vs (summary) response variant is response-shape,
	// not a separate permission — every role holds `node:read`.
	PermNodeRead        Permission = "node:read"
	PermNodeMaintenance Permission = "node:maintenance"
	PermNodeManage      Permission = "node:manage"

	// Users and API tokens. `api_token:manage` is `any` for admin and
	// `own` for every other role — the admin-on-behalf endpoints under
	// /v1/users/{id}/api-tokens lean on the standard own/any check via
	// auth.CheckOwnership rather than a special-case admin branch.
	PermUserRead       Permission = "user:read"
	PermUserManage     Permission = "user:manage"
	PermAPITokenManage Permission = "api_token:manage"

	// Tasks. The "owner" of a task is the user that triggered it.
	PermTaskRead   Permission = "task:read"
	PermTaskCancel Permission = "task:cancel"

	// Cluster-level configuration. Reads (GET /v1/cluster/default-pool,
	// future cluster setting endpoints) are open to every authenticated
	// role — the default-pool name is operator-facing context, not a
	// secret. Mutations are admin-only by precedent with other
	// cluster-shaping permissions (storage_pool:manage, node:manage).
	PermClusterRead   Permission = "cluster:read"
	PermClusterManage Permission = "cluster:manage"
)

// Scope is the slice of resources a Permission applies to for a given
// Role. Exactly three values exist; ScopeNone is the zero value
// returned by ScopeFor when the role does not hold the permission, so
// callers can switch on the result without a separate "ok" boolean.
type Scope string

const (
	// ScopeNone is the absence of a permission for a role.
	ScopeNone Scope = ""

	// ScopeOwn limits a permission to resources whose owner_id equals
	// the caller's user id, or to actions on the caller's own data
	// (own snapshots, own tasks, own API tokens). Resources without an
	// owner_id column must not be paired with ScopeOwn — see
	// CheckOwnership.
	ScopeOwn Scope = "own"

	// ScopeAny grants the permission across all matching resources.
	// For permissions on ownerless resources (networks, storage pools,
	// firmwares, nodes), ScopeAny is the only meaningful "permission
	// held" value.
	ScopeAny Scope = "any"
)

// permissionMatrix is the static (Role, Permission) -> Scope table.
// Source of truth: docs/rbac.md. Each row is enumerated explicitly
// (no implicit "admin extends operator extends developer") to keep
// diffs auditable and to make missing-entry mistakes loud.
var permissionMatrix = map[Role]map[Permission]Scope{
	RoleAdmin: {
		// VM lifecycle — full control across the fleet.
		PermVMRead:         ScopeAny,
		PermVMCreate:       ScopeAny,
		PermVMUpdate:       ScopeAny,
		PermVMDelete:       ScopeAny,
		PermVMLifecycle:    ScopeAny,
		PermVMResize:       ScopeAny,
		PermVMConsole:      ScopeAny,
		PermVMRevert:       ScopeAny,
		PermVMMigrate:      ScopeAny,
		PermVMSSH:          ScopeAny,
		PermVMIngressGrant: ScopeAny,
		PermVMConnect:      ScopeAny,

		// Load balancers.
		PermLoadBalancerRead:    ScopeAny,
		PermLoadBalancerCreate:  ScopeAny,
		PermLoadBalancerUpdate:  ScopeAny,
		PermLoadBalancerDelete:  ScopeAny,
		PermLoadBalancerConnect: ScopeAny,

		// Snapshots.
		PermSnapshotRead:   ScopeAny,
		PermSnapshotCreate: ScopeAny,
		PermSnapshotDelete: ScopeAny,
		PermSnapshotRevert: ScopeAny,

		// Infrastructure.
		PermNetworkRead:       ScopeAny,
		PermNetworkManage:     ScopeAny,
		PermStoragePoolRead:   ScopeAny,
		PermStoragePoolManage: ScopeAny,
		PermStoragePoolScan:   ScopeAny,
		PermFirmwareRead:      ScopeAny,
		PermFirmwareManage:    ScopeAny,

		// Nodes.
		PermNodeRead:        ScopeAny,
		PermNodeMaintenance: ScopeAny,
		PermNodeManage:      ScopeAny,

		// Users and tokens. `api_token:manage` is `any` for admin so the
		// admin-on-behalf endpoints (POST/GET /v1/users/{id}/api-tokens,
		// DELETE /v1/users/{id}/api-tokens/{token_id}) compose with the
		// shared own/any pattern via auth.CheckOwnership; non-admin roles
		// stay scope=own and a non-admin caller targeting another user
		// gets 404 from the handler (no existence leak).
		PermUserRead:       ScopeAny,
		PermUserManage:     ScopeAny,
		PermAPITokenManage: ScopeAny,

		// Tasks.
		PermTaskRead:   ScopeAny,
		PermTaskCancel: ScopeAny,

		// Cluster configuration.
		PermClusterRead:   ScopeAny,
		PermClusterManage: ScopeAny,
	},

	RoleOperator: {
		// VM lifecycle — operator manages on behalf of users, hence
		// scope `any`.
		PermVMRead:         ScopeAny,
		PermVMCreate:       ScopeAny,
		PermVMUpdate:       ScopeAny,
		PermVMDelete:       ScopeAny,
		PermVMLifecycle:    ScopeAny,
		PermVMResize:       ScopeAny,
		PermVMConsole:      ScopeAny,
		PermVMRevert:       ScopeAny,
		PermVMMigrate:      ScopeAny,
		PermVMSSH:          ScopeAny,
		PermVMIngressGrant: ScopeAny,
		PermVMConnect:      ScopeAny,

		// Load balancers.
		PermLoadBalancerRead:    ScopeAny,
		PermLoadBalancerCreate:  ScopeAny,
		PermLoadBalancerUpdate:  ScopeAny,
		PermLoadBalancerDelete:  ScopeAny,
		PermLoadBalancerConnect: ScopeAny,

		// Snapshots.
		PermSnapshotRead:   ScopeAny,
		PermSnapshotCreate: ScopeAny,
		PermSnapshotDelete: ScopeAny,
		PermSnapshotRevert: ScopeAny,

		// Infrastructure — read everywhere; admin-only for
		// network/pool/firmware lifecycle.
		PermNetworkRead:     ScopeAny,
		PermStoragePoolRead: ScopeAny,
		PermStoragePoolScan: ScopeAny,
		PermFirmwareRead:    ScopeAny,

		// Nodes — read and operational maintenance, but not lifecycle.
		PermNodeRead:        ScopeAny,
		PermNodeMaintenance: ScopeAny,

		// Users — read for support, no lifecycle.
		PermUserRead:       ScopeAny,
		PermAPITokenManage: ScopeOwn,

		// Tasks.
		PermTaskRead:   ScopeAny,
		PermTaskCancel: ScopeAny,

		// Cluster configuration — read only; only admin reshapes the
		// cluster.
		PermClusterRead: ScopeAny,
	},

	RoleDeveloper: {
		// VM lifecycle — read every VM (names/metadata are not secret),
		// mutate only own. No migration.
		PermVMRead:         ScopeAny,
		PermVMCreate:       ScopeAny,
		PermVMUpdate:       ScopeOwn,
		PermVMDelete:       ScopeOwn,
		PermVMLifecycle:    ScopeOwn,
		PermVMResize:       ScopeOwn,
		PermVMConsole:      ScopeOwn,
		PermVMRevert:       ScopeOwn,
		PermVMSSH:          ScopeOwn,
		PermVMIngressGrant: ScopeOwn,
		PermVMConnect:      ScopeOwn,

		// Load balancers — read/create fleet-wide, mutate/connect own only.
		PermLoadBalancerRead:    ScopeAny,
		PermLoadBalancerCreate:  ScopeAny,
		PermLoadBalancerUpdate:  ScopeOwn,
		PermLoadBalancerDelete:  ScopeOwn,
		PermLoadBalancerConnect: ScopeOwn,

		// Snapshots — own only.
		PermSnapshotRead:   ScopeOwn,
		PermSnapshotCreate: ScopeOwn,
		PermSnapshotDelete: ScopeOwn,
		PermSnapshotRevert: ScopeOwn,

		// Infrastructure — read-only for context.
		PermNetworkRead:     ScopeAny,
		PermStoragePoolRead: ScopeAny,
		PermFirmwareRead:    ScopeAny,

		// Nodes — summary read.
		PermNodeRead: ScopeAny,

		// Self-service tokens.
		PermAPITokenManage: ScopeOwn,

		// Tasks — own only.
		PermTaskRead:   ScopeOwn,
		PermTaskCancel: ScopeOwn,

		// Cluster configuration — read only.
		PermClusterRead: ScopeAny,
	},

	RoleViewer: {
		// Read-only across the visible surface.
		PermVMRead: ScopeAny,

		// Load balancers — read only.
		PermLoadBalancerRead: ScopeAny,

		// Snapshots — own only (anticipates a future flow where viewers
		// trigger read-side operations that emit task/snapshot rows
		// against their own id).
		PermSnapshotRead: ScopeOwn,

		// Infrastructure — read-only.
		PermNetworkRead:     ScopeAny,
		PermStoragePoolRead: ScopeAny,
		PermFirmwareRead:    ScopeAny,

		// Nodes — summary read.
		PermNodeRead: ScopeAny,

		// Self-service tokens.
		PermAPITokenManage: ScopeOwn,

		// Tasks — own only; no cancel.
		PermTaskRead: ScopeOwn,

		// Cluster configuration — read only.
		PermClusterRead: ScopeAny,
	},
}

// ScopeFor returns the scope at which role holds perm, or ScopeNone
// when the role does not hold the permission. Unknown roles also
// return ScopeNone — matrix lookups for an unrecognised role yield no
// permissions, which is the safe default.
func ScopeFor(role Role, perm Permission) Scope {
	perms, ok := permissionMatrix[role]
	if !ok {
		return ScopeNone
	}
	return perms[perm]
}

// Has reports whether role holds perm at any scope.
func Has(role Role, perm Permission) bool {
	return ScopeFor(role, perm) != ScopeNone
}
