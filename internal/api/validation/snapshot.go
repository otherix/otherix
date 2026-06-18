// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"errors"
	"regexp"
)

// SnapshotNameMaxLength bounds snapshot names and matches
// `SnapshotName.maxLength` in the OpenAPI contracts. The store column is
// unbounded; the cap is API-edge only.
const SnapshotNameMaxLength = 255

// ErrInvalidSnapshotName is returned by ValidateSnapshotName for an empty,
// over-length, or disallowed-charset name. Handlers map it to 400
// validation_failed.
var ErrInvalidSnapshotName = errors.New("snapshot name must be 1-255 chars, start alphanumeric, and contain only [A-Za-z0-9._-]")

// snapshotNamePattern is deliberately strict: the name is concatenated into an
// agent-side filesystem path (`{vm_uuid}__{name}.json` manifest, and indirectly
// the per-blob files), so any path separator or traversal token must be rejected
// at the edge. Requiring the first rune to be alphanumeric also blocks leading
// dots/dashes; the absence of '/' and '\' from the class makes a path-segment
// traversal (`../`) unrepresentable.
var snapshotNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateSnapshotName enforces the snapshot-name contract: 1..255 chars and the
// strict charset above. It is the primary guard against an attacker steering an
// agent filesystem path via a crafted name (path traversal / arbitrary file
// delete); the agent re-validates as defense in depth.
func ValidateSnapshotName(name string) error {
	if name == "" || len([]rune(name)) > SnapshotNameMaxLength {
		return ErrInvalidSnapshotName
	}
	if !snapshotNamePattern.MatchString(name) {
		return ErrInvalidSnapshotName
	}
	return nil
}
