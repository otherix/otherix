// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

// Advisory-lock key namespace for HA-safe api-server coordination.
//
// Otherix api-server is deployed multi-replica for HA. Operations that
// read mutable state and act on it in a separate write (placement is the
// canonical case — pick a node, then insert a VM pinned to it) must
// serialize their critical section across replicas, otherwise concurrent
// decisions can over-allocate. The key namespace identifies each such
// critical section; AcquirePlacementLock is a no-op on the single-node
// default, since an etcd transaction already serializes the write.
//
// Contract:
//
//   - Every key here is acquired ONLY inside a transaction.
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
	// LockKeyPlacement gates the VM placement decision window. The
	// vm.create handler acquires it inside the same etcd transaction that
	// runs SchedulePlacement + CreateVM + CreateVMDisk + CreateTask +
	// the job enqueue, so concurrent placements across api-server
	// replicas observe each other's pin before they read candidate
	// availability.
	LockKeyPlacement int64 = 1
)
