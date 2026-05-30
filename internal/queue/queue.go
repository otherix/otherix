// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package queue is the control plane's backend-agnostic job-queue
// boundary. Handlers enqueue and cancel background work through the
// Enqueuer interface without knowing which queue implementation backs
// it. Today the only implementation is river-backed (see
// internal/queue/riverqueue); Phase 3 swaps in an etcd-watch
// implementation behind the same interface, so handlers and the store's
// transactional enqueue path do not change.
//
// The job payload types live in the handler packages that own the
// corresponding worker; they satisfy JobArgs (and, in the river era,
// river.JobArgs as well, since both interfaces are just Kind()).
package queue

import "context"

// JobArgs is a backend-agnostic job descriptor. Kind names the job
// type. The concrete payload structs are defined by the worker-owning
// packages; in the river era they double as river.JobArgs (the method
// set is identical), so no remarshaling happens at the boundary.
type JobArgs interface {
	Kind() string
}

// JobRef is an opaque handle to an enqueued job, used to cancel it. In
// the river era it wraps the int64 river job id persisted in
// tasks.river_job_id; Phase 3's etcd backend will repopulate it from an
// etcd key (the column migrates with the backend). Callers treat it as
// opaque and only round-trip it through the tasks row.
type JobRef struct {
	// ID is the backend job identifier. River era: the int64 river job
	// id. Zero means "no job" (e.g. a task that was never stamped).
	ID int64
}

// Enqueuer enqueues and cancels background jobs within an ambient
// transaction. Instances are bound to a specific transaction by the
// store (see store.InTxEnqueue) so the job and the task row commit
// atomically; no driver or river types appear in this interface. The
// concrete JobArgs payloads still have to be serializable by whatever
// backend is wired in (the river adapter reflects the concrete type in
// InsertTx; a Phase-3 etcd backend would marshal the same structs its
// own way).
type Enqueuer interface {
	// Enqueue submits args for background execution and returns a
	// reference the caller can persist and later Cancel.
	Enqueue(ctx context.Context, args JobArgs) (JobRef, error)
	// Cancel best-effort cancels a previously enqueued job. Cancelling
	// an already-finished or unknown job is the backend's call.
	Cancel(ctx context.Context, ref JobRef) error
}
