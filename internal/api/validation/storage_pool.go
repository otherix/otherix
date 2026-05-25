// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// StoragePoolNameMaxLength bounds storage_pools.name and matches
// `StoragePool.name.maxLength` in api/openapi/control-plane.yaml. The
// SQL column is unbounded text; the cap is API-edge only.
const StoragePoolNameMaxLength = 255

// PoolPathMaxLength bounds storage_pools.path. The kernel allows
// PATH_MAX (4096 on Linux) but most filesystems and tooling start
// misbehaving well before that; 1024 is a common practical ceiling
// and matches the OpenAPI guidance for path-shaped strings.
const PoolPathMaxLength = 1024

// StoragePoolTypeLocalDir is the canonical pool-type value.
// Mirrors the agent OpenAPI StoragePoolReportType enum and the
// (text + check) constraint on storage_pools.type. Future pool
// backends widen the set with their own constants; the SQL CHECK is
// the source of truth, this constant is the API-edge mirror.
const StoragePoolTypeLocalDir = "local_dir"

// ValidateStoragePoolType returns nil when t is a recognised pool
// type. Currently supports only "local_dir"; future backends
// (`lvm-thin`, `nfs`, …) extend this branch alongside the migration's
// CHECK constraint.
func ValidateStoragePoolType(t string) error {
	if t == StoragePoolTypeLocalDir {
		return nil
	}
	return fmt.Errorf("invalid storage pool type %q (must be one of: %s)", t, StoragePoolTypeLocalDir)
}

// ValidateStoragePoolName returns nil when s is a syntactically valid
// pool name. The rules mirror network and node naming: 1..255 runes,
// no leading/trailing whitespace. The `(node_id, name)` partial unique
// index in the schema enforces the global uniqueness invariant; this
// helper only catches malformed inputs.
func ValidateStoragePoolName(s string) error {
	if s == "" {
		return errors.New("name is required")
	}
	if s != strings.TrimSpace(s) {
		return errors.New("name must not have leading or trailing whitespace")
	}
	if utf8.RuneCountInString(s) > StoragePoolNameMaxLength {
		return fmt.Errorf("name is too long (max %d runes)", StoragePoolNameMaxLength)
	}
	return nil
}

// ValidatePoolPath returns nil when s is a syntactically valid
// absolute POSIX path: 1..PoolPathMaxLength bytes, starts with `/`, no
// embedded NUL byte. The validator does NOT canonicalise — the path is
// part of the contract with the agent and shipping a stricter form
// (no trailing slash, collapsed `..`, …) than the operator typed
// would surprise them.
func ValidatePoolPath(s string) error {
	if s == "" {
		return errors.New("path is required")
	}
	if len(s) > PoolPathMaxLength {
		return fmt.Errorf("path is too long (max %d bytes)", PoolPathMaxLength)
	}
	if !strings.HasPrefix(s, "/") {
		return fmt.Errorf("path must be absolute (start with %q)", "/")
	}
	if strings.ContainsRune(s, 0) {
		return errors.New("path must not contain the NUL byte")
	}
	return nil
}

// ErrPoolPathNotAllowed is returned by ValidatePoolPathAgainstAllowlist
// when the supplied path does not fall under any of the configured
// prefixes. Sentinel so the handler can render the dedicated
// `path_not_allowed` error code rather than the generic
// `validation_failed`.
var ErrPoolPathNotAllowed = errors.New("path is not on the storage_pools allowlist")

// ValidatePoolPathAgainstAllowlist returns nil when path is a prefix
// of one of the operator-configured allowed prefixes. This gate
// prevents POST /v1/storage-pools from targeting filesystem locations
// the operator has not opted into (default allowlist
// `/opt/otherix/pools/`).
//
// Trailing-slash invariant: prefixes are validated to end in `/` so
// `/opt/otherix/pools` cannot match `/opt/otherix/pools-evil/`.
// Callers MUST run ValidatePoolPath first — this gate trusts the
// path to already be syntactically valid (absolute, NUL-free).
func ValidatePoolPathAgainstAllowlist(path string, prefixes []string) error {
	// path is already validated by ValidatePoolPath — absolute, NUL-free.
	// Ensure path itself ends in `/` for the substring match so
	// `/opt/otherix/pools` matches `/opt/otherix/pools/`.
	candidate := path
	if !strings.HasSuffix(candidate, "/") {
		candidate += "/"
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(candidate, prefix) {
			return nil
		}
	}
	return ErrPoolPathNotAllowed
}
