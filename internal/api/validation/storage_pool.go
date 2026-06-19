// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
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

// storagePoolNameRe restricts a pool name to start with an alphanumeric
// and continue with alphanumerics, dot, dash, or underscore - notably no
// '/' (the etcd key separator), so a name cannot poison the pool-name
// prefix-scan index.
var storagePoolNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateStoragePoolName returns nil when s is a syntactically valid
// pool name: 1..255 runes matching [A-Za-z0-9][A-Za-z0-9._-]*. The
// charset deliberately excludes '/' (and all whitespace, separators,
// and control characters) - the pool-name secondary index is consumed
// by a prefix scan, so a '/' in a name would fold one pool's index
// entries into another's. The `(node_id, name)` partial unique index
// in the schema enforces the global uniqueness invariant; this helper
// only catches malformed inputs.
func ValidateStoragePoolName(s string) error {
	if s == "" {
		return errors.New("name is required")
	}
	if utf8.RuneCountInString(s) > StoragePoolNameMaxLength {
		return fmt.Errorf("name is too long (max %d runes)", StoragePoolNameMaxLength)
	}
	if !storagePoolNameRe.MatchString(s) {
		return fmt.Errorf("invalid pool name %q (must match [A-Za-z0-9][A-Za-z0-9._-]*)", s)
	}
	return nil
}

// ValidatePoolPath returns nil when s is a syntactically valid
// absolute POSIX path: 1..PoolPathMaxLength bytes, starts with `/`, no
// embedded NUL byte, and no `..` path segment. The validator does NOT
// canonicalise cosmetic forms (trailing slash, `//`) - the path is
// part of the contract with the agent and shipping a stricter form
// than the operator typed would surprise them - but a `..` traversal
// segment is never a legitimate pool root and would let the path
// escape the allowlist gate, so it is rejected outright rather than
// silently rewritten.
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
	for _, segment := range strings.Split(s, "/") {
		if segment == ".." {
			return errors.New("path must not contain a '..' segment")
		}
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
// `/var/lib/otherix/pools/`).
//
// Trailing-slash invariant: prefixes are validated to end in `/` so
// `/var/lib/otherix/pools` cannot match `/var/lib/otherix/pools-evil/`.
// The candidate is canonicalised with filepath.Clean before matching,
// so a `..` traversal form (`/var/lib/otherix/pools/../../../etc`)
// cannot HasPrefix-match its way past the gate - the gate
// is self-protecting regardless of whether the caller ran
// ValidatePoolPath first. Callers SHOULD still run ValidatePoolPath
// first for the full syntactic checks (NUL-free, length cap).
func ValidatePoolPathAgainstAllowlist(path string, prefixes []string) error {
	// Canonicalise before matching: collapse `..` and `//` so the prefix
	// match operates on the path the filesystem would actually resolve.
	// filepath.Clean strips any trailing slash, so re-append `/` for the
	// substring match - `/var/lib/otherix/pools` must still match the
	// prefix `/var/lib/otherix/pools/`.
	candidate := filepath.Clean(path) + "/"
	for _, prefix := range prefixes {
		// Clean the prefix too so a cosmetically-sloppy operator prefix
		// (`/var/lib//pools/`) still matches a cleaned candidate, matching
		// the defaultpool guard's behavior.
		if strings.HasPrefix(candidate, filepath.Clean(prefix)+"/") {
			return nil
		}
	}
	return ErrPoolPathNotAllowed
}
