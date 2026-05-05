// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

// Advisory-lock key namespace for HA-safe api-server coordination.
//
// Otherix api-server is deployed multi-replica for HA. Operations that
// read mutable state и act on it in а separate write (placement is the
// canonical case — pick а node, then insert а VM pinned to it) must
// serialize their critical section across replicas, otherwise concurrent
// decisions can over-allocate. Postgres advisory transaction locks
// (`pg_advisory_xact_lock`) are the chosen primitive: cluster-scoped,
// released automatically on commit/rollback, no extra infrastructure.
//
// Contract:
//
//   - Every key here is acquired ONLY inside а transaction. Calling
//     AcquirePlacementLock outside an InTxWithTx callback acquires the
//     lock и immediately releases it on the same statement (pgx
//     autocommit), which serializes nothing.
//   - Keys are bigint, namespaced by reservation in this file. Reserve
//     before use, never reuse а freed key (the symbol stays, even if
//     the workflow that minted it goes away — an in-flight transaction
//     under а recycled key would silently coordinate with the wrong
//     thing).
//   - Range 1..99 is reserved for core scheduler / placement / cluster-
//     wide singleton workflows. 100..999 is reserved for future domain-
//     scoped locks (per-pool, per-node, etc.) if granularity gains
//     justify the additional surface.
const (
	// LockKeyPlacement gates the VM placement decision window. The
	// vm.create handler acquires it inside the same InTxWithTx that
	// runs SchedulePlacement + CreateVM + CreateVMDisk + CreateTask +
	// river.InsertTx, so concurrent placements across api-server
	// replicas observe each other's pin before they read candidate
	// availability.
	LockKeyPlacement int64 = 1
)
