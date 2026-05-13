// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth

import (
	"fmt"

	"github.com/google/uuid"
)

// CheckOwnership decides whether user may perform perm on a resource
// owned by resourceOwner. Returns nil when allowed, ErrPermissionDenied
// when the role lacks the permission or scope=own does not match.
//
// resourceOwner is nil for ownerless resources (networks, storage pools,
// firmwares, nodes); pairing nil with a scope=own permission is a
// matrix bug and surfaces as a non-sentinel error so it shows up in
// logs rather than masquerading as a normal authz failure.
func CheckOwnership(user *User, resourceOwner *uuid.UUID, perm Permission) error {
	if user == nil {
		return ErrPermissionDenied
	}

	switch ScopeFor(user.Role, perm) {
	case ScopeAny:
		return nil

	case ScopeOwn:
		if resourceOwner == nil {
			return fmt.Errorf("permission %q with scope=own applied to ownerless resource", perm)
		}
		if user.ID == *resourceOwner {
			return nil
		}
		return ErrPermissionDenied

	default:
		return ErrPermissionDenied
	}
}
