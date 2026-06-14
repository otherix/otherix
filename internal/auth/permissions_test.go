// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth_test

import (
	"testing"

	"github.com/otherix/otherix/internal/auth"
)

// allPermissions enumerates every Permission constant defined in
// permissions.go. Hand-maintained alongside the const block so that
// adding a permission without considering each role fails a test.
var allPermissions = []auth.Permission{
	auth.PermVMRead, auth.PermVMCreate, auth.PermVMUpdate, auth.PermVMDelete,
	auth.PermVMLifecycle, auth.PermVMResize, auth.PermVMConsole, auth.PermVMRevert,
	auth.PermVMMigrate,

	auth.PermSnapshotRead, auth.PermSnapshotCreate, auth.PermSnapshotDelete,
	auth.PermSnapshotRevert,

	auth.PermNetworkRead, auth.PermNetworkManage,
	auth.PermStoragePoolRead, auth.PermStoragePoolManage, auth.PermStoragePoolScan,
	auth.PermFirmwareRead, auth.PermFirmwareManage,

	auth.PermNodeRead, auth.PermNodeMaintenance, auth.PermNodeManage,

	auth.PermUserRead, auth.PermUserManage, auth.PermAPITokenManage,

	auth.PermTaskRead, auth.PermTaskCancel,

	auth.PermClusterRead, auth.PermClusterManage,
}

var allRoles = []auth.Role{auth.RoleAdmin, auth.RoleOperator, auth.RoleDeveloper, auth.RoleViewer}

func TestMatrix_AdminHasEveryPermission(t *testing.T) {
	for _, p := range allPermissions {
		if !auth.Has(auth.RoleAdmin, p) {
			t.Errorf("admin missing permission %q", p)
		}
	}
}

func TestMatrix_AllRolesPopulated(t *testing.T) {
	for _, role := range allRoles {
		if !roleHasAny(role) {
			t.Errorf("role %q has no permissions in matrix", role)
		}
	}
}

func roleHasAny(role auth.Role) bool {
	for _, p := range allPermissions {
		if auth.Has(role, p) {
			return true
		}
	}
	return false
}

func TestMatrix_ViewerIsReadOnly(t *testing.T) {
	writePerms := []auth.Permission{
		auth.PermVMCreate, auth.PermVMUpdate, auth.PermVMDelete,
		auth.PermVMLifecycle, auth.PermVMResize, auth.PermVMConsole,
		auth.PermVMRevert, auth.PermVMMigrate,
		auth.PermSnapshotCreate, auth.PermSnapshotDelete, auth.PermSnapshotRevert,
		auth.PermNetworkManage,
		auth.PermStoragePoolManage, auth.PermStoragePoolScan,
		auth.PermFirmwareManage,
		auth.PermNodeMaintenance, auth.PermNodeManage,
		auth.PermUserManage,
		auth.PermTaskCancel,
		auth.PermClusterManage,
	}
	for _, p := range writePerms {
		if auth.Has(auth.RoleViewer, p) {
			t.Errorf("viewer holds unexpected write permission %q", p)
		}
	}
}

func TestMatrix_DeveloperOwnScope(t *testing.T) {
	cases := []struct {
		perm auth.Permission
		want auth.Scope
	}{
		{auth.PermVMUpdate, auth.ScopeOwn},
		{auth.PermVMDelete, auth.ScopeOwn},
		{auth.PermVMLifecycle, auth.ScopeOwn},
		{auth.PermVMResize, auth.ScopeOwn},
		{auth.PermVMConsole, auth.ScopeOwn},
		{auth.PermVMRevert, auth.ScopeOwn},

		{auth.PermSnapshotRead, auth.ScopeOwn},
		{auth.PermSnapshotCreate, auth.ScopeOwn},
		{auth.PermSnapshotDelete, auth.ScopeOwn},
		{auth.PermSnapshotRevert, auth.ScopeOwn},

		{auth.PermAPITokenManage, auth.ScopeOwn},

		{auth.PermTaskRead, auth.ScopeOwn},
		{auth.PermTaskCancel, auth.ScopeOwn},
	}
	for _, tc := range cases {
		if got := auth.ScopeFor(auth.RoleDeveloper, tc.perm); got != tc.want {
			t.Errorf("developer scope for %q = %q, want %q", tc.perm, got, tc.want)
		}
	}
}

func TestMatrix_DeveloperForbidden(t *testing.T) {
	forbidden := []auth.Permission{
		auth.PermVMMigrate,
		auth.PermNetworkManage,
		auth.PermStoragePoolManage, auth.PermStoragePoolScan,
		auth.PermFirmwareManage,
		auth.PermNodeMaintenance, auth.PermNodeManage,
		auth.PermUserRead, auth.PermUserManage,
		auth.PermClusterManage,
	}
	for _, p := range forbidden {
		if auth.Has(auth.RoleDeveloper, p) {
			t.Errorf("developer holds forbidden permission %q", p)
		}
	}
}

func TestMatrix_OperatorRestrictions(t *testing.T) {
	adminOnly := []auth.Permission{
		auth.PermNetworkManage,
		auth.PermStoragePoolManage,
		auth.PermFirmwareManage,
		auth.PermNodeManage,
		auth.PermUserManage,
		auth.PermClusterManage,
	}
	for _, p := range adminOnly {
		if auth.Has(auth.RoleOperator, p) {
			t.Errorf("operator holds admin-only permission %q", p)
		}
	}

	anyScopeOperator := []auth.Permission{
		auth.PermVMUpdate, auth.PermVMDelete, auth.PermVMLifecycle,
		auth.PermVMResize, auth.PermVMConsole, auth.PermVMRevert,
		auth.PermVMMigrate,
		auth.PermSnapshotCreate, auth.PermSnapshotDelete, auth.PermSnapshotRevert,
		auth.PermNodeMaintenance,
	}
	for _, p := range anyScopeOperator {
		if got := auth.ScopeFor(auth.RoleOperator, p); got != auth.ScopeAny {
			t.Errorf("operator scope for %q = %q, want any", p, got)
		}
	}
}

func TestMatrix_APITokenManageAdminAnyOthersOwn(t *testing.T) {
	cases := map[auth.Role]auth.Scope{
		auth.RoleAdmin:     auth.ScopeAny,
		auth.RoleOperator:  auth.ScopeOwn,
		auth.RoleDeveloper: auth.ScopeOwn,
		auth.RoleViewer:    auth.ScopeOwn,
	}
	for role, want := range cases {
		if got := auth.ScopeFor(role, auth.PermAPITokenManage); got != want {
			t.Errorf("%s scope for api_token:manage = %q, want %q", role, got, want)
		}
	}
}

func TestMatrix_VMCreateIsImageGate(t *testing.T) {
	// vm:create is the single gate for materializing a VM from an image
	// URL; there is no per-image authorization. admin, operator, and
	// developer all hold it at ScopeAny (a developer may create a VM from
	// any image URL they supply); viewer does not hold it at all.
	cases := map[auth.Role]auth.Scope{
		auth.RoleAdmin:     auth.ScopeAny,
		auth.RoleOperator:  auth.ScopeAny,
		auth.RoleDeveloper: auth.ScopeAny,
		auth.RoleViewer:    auth.ScopeNone,
	}
	for role, want := range cases {
		if got := auth.ScopeFor(role, auth.PermVMCreate); got != want {
			t.Errorf("%s scope for vm:create = %q, want %q", role, got, want)
		}
	}
}

func TestMatrix_VMMigrateAdminOperatorOnly(t *testing.T) {
	// vm:migrate is operator-grade: admin and operator hold it at
	// ScopeAny; developer and viewer do not hold it at all (the canonical
	// 403-for-developer rule - a developer sees their own VM but cannot
	// migrate it). docs/rbac.md VM table is the human-readable contract.
	if !auth.Has(auth.RoleAdmin, auth.PermVMMigrate) {
		t.Errorf("admin missing vm:migrate")
	}
	if !auth.Has(auth.RoleOperator, auth.PermVMMigrate) {
		t.Errorf("operator missing vm:migrate")
	}
	if auth.Has(auth.RoleDeveloper, auth.PermVMMigrate) {
		t.Errorf("developer holds vm:migrate, want none")
	}
	if auth.Has(auth.RoleViewer, auth.PermVMMigrate) {
		t.Errorf("viewer holds vm:migrate, want none")
	}
	if got := auth.ScopeFor(auth.RoleAdmin, auth.PermVMMigrate); got != auth.ScopeAny {
		t.Errorf("admin scope for vm:migrate = %q, want any", got)
	}
	if got := auth.ScopeFor(auth.RoleOperator, auth.PermVMMigrate); got != auth.ScopeAny {
		t.Errorf("operator scope for vm:migrate = %q, want any", got)
	}
}

func TestMatrix_UnknownRoleHasNoPermissions(t *testing.T) {
	bogus := auth.Role("ghost")
	for _, p := range allPermissions {
		if auth.Has(bogus, p) {
			t.Errorf("unknown role holds permission %q", p)
		}
		if got := auth.ScopeFor(bogus, p); got != auth.ScopeNone {
			t.Errorf("unknown role scope for %q = %q, want %q", p, got, auth.ScopeNone)
		}
	}
}
