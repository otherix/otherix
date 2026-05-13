// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import "fmt"

// Role is the RBAC anchor for a user. The wire format (JWT `role` claim,
// users.role column, JSON) stays a lowercase string; conversions happen
// at the boundary via ParseRole. The four valid roles are admin,
// operator, developer, viewer; see docs/rbac.md.
type Role string

// The four defined roles. See docs/rbac.md.
const (
	RoleAdmin     Role = "admin"
	RoleOperator  Role = "operator"
	RoleDeveloper Role = "developer"
	RoleViewer    Role = "viewer"
)

// Valid reports whether r is one of the defined roles.
func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleOperator, RoleDeveloper, RoleViewer:
		return true
	}
	return false
}

// ParseRole converts a wire-format string into a typed Role and returns
// an error for values outside the enum. Use at every boundary that
// crosses untyped data (token verification, DB row mapping) so an
// invalid Role never lands in a User struct.
func ParseRole(s string) (Role, error) {
	r := Role(s)
	if !r.Valid() {
		return "", fmt.Errorf("unknown role %q", s)
	}
	return r, nil
}
