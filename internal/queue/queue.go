// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package queue is the control plane's job-queue boundary type. Handler
// packages own the concrete job payload structs (one per worker kind) and the
// store enqueues them through the etcd-backed queue; this package holds only the
// shared JobArgs descriptor so the handler payloads stay decoupled from the
// store's enqueue path.
package queue

// JobArgs is a job descriptor: Kind names the job type. The concrete payload
// structs are defined by the worker-owning handler packages; the store
// JSON-marshals them onto the etcd job key.
type JobArgs interface {
	Kind() string
}
