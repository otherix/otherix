// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

// Advisory-lock key namespace for api-server coordination.
//
// Operations that read mutable state and act on it in a separate write
// (placement is the canonical case — pick a node, then insert a VM pinned to
// it) must serialize their critical section, otherwise concurrent decisions
// can over-allocate. The key namespace identifies each such critical section.
// AcquirePlacementLock is process-local (a keyed mutex, see
// internal/etcdstore/placement_lock.go): it serializes placement inside the
// worker leader; cross-replica exclusion comes from leader-only workers and
// the leader fence on the bind txn.
//
// Contract:
//
//   - Each key is acquired in the handler around its read->commit window
//     (the placement read and the committing bind), held until that window
//     completes.
//   - Keys are bigint, namespaced by reservation in this file. Reserve
//     before use, never reuse a freed key (the symbol stays, even if
//     the workflow that minted it goes away — an in-flight transaction
//     under a recycled key would silently coordinate with the wrong
//     thing).
//   - Range 1..99 is reserved for core scheduler / placement / cluster-
//     wide singleton workflows. 100..999 is reserved for future domain-
//     scoped locks (per-pool, per-node, etc.) if granularity gains
//     justify the additional surface.
const (
	// LockKeyPlacement gates the VM placement decision window. The vm.create
	// scheduling loop (bindWithMACRetry) acquires it around BindScheduledVM,
	// and the migration worker acquires it around placer.Place ->
	// BindMigrationTarget, so concurrent placements observe each other's pin
	// before they read candidate availability.
	LockKeyPlacement int64 = 1
)
