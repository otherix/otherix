// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package reconciler hosts the agent's per-resource reconciliation
// goroutines. Each reconciler subscribes to heartbeat responses
// through а narrow ResponseHandler interface, publishes desired state
// into an atomic.Pointer slot, and runs а tick loop that diffs
// desired vs observed and applies changes against the owning manager
// (vm.Manager today).
//
// Concurrency model:
//
//   - Desired-state cache is а single atomic.Pointer slot; the
//     heartbeat sender stores а new slice on every tick, the
//     reconciler loads it wait-free.
//   - А buffered (size 1) "trigger" channel coalesces nudges. The
//     heartbeat sender does а non-blocking send after each response;
//     the reconciler's select drains it. Combined with the periodic
//     ticker, the reconciler reacts within а heartbeat tick OR within
//     the periodic interval, whichever comes first.
//   - Pool registry mutations (manager.AddPool / RemovePool) take
//     the manager's poolsMu RWMutex; read-mostly VM ops want RLock,
//     so reconciliation churn does not block them.
//
// Failure semantics: retry forever. А `mkdir -p` that fails (path
// permission, filesystem full) is reported as `failed` on the next
// heartbeat и retried on every subsequent tick без backoff. There
// is no retry-count cap; operator visibility comes от the
// `reconciliation_status` column на the CP side.
//
// Resource scope в this iteration: storage pools only. VM lifecycle
// reconciliation is the planned next slice and follows the same
// skeleton — а dedicated reconciler subscribed to the same heartbeat
// channel, owning its own `desired_vms[]` slot.
package reconciler
