// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package riverqueue is the river-backed implementation of the
// control plane's queue boundary (internal/queue). It is the single
// place that knows about river on the producer side: it wraps a
// *river.Client and binds it to a pgx transaction so the store's
// InTxEnqueue path can enqueue a job atomically with the task row.
//
// Phase 3 replaces this package with an etcd-watch implementation of
// the same store.QueueBinder / queue.Enqueuer contract; nothing else
// changes.
package riverqueue

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/otherix/otherix/internal/queue"
)

// Binder wraps a river client and produces transaction-bound
// queue.Enqueuer values. It satisfies store.QueueBinder.
type Binder struct {
	client *river.Client[pgx.Tx]
}

// New returns a Binder over the given river client.
func New(client *river.Client[pgx.Tx]) *Binder {
	return &Binder{client: client}
}

// Bind returns a queue.Enqueuer that enqueues within tx.
func (b *Binder) Bind(tx pgx.Tx) queue.Enqueuer {
	return txEnqueuer{client: b.client, tx: tx}
}

// txEnqueuer is a queue.Enqueuer bound to a single pgx transaction.
type txEnqueuer struct {
	client *river.Client[pgx.Tx]
	tx     pgx.Tx
}

// Enqueue inserts the job in the bound transaction. args satisfies
// river.JobArgs structurally (both interfaces are just Kind()), so it
// passes straight through with no conversion.
func (e txEnqueuer) Enqueue(ctx context.Context, args queue.JobArgs) (queue.JobRef, error) {
	res, err := e.client.InsertTx(ctx, e.tx, args, nil)
	if err != nil {
		return queue.JobRef{}, err
	}
	return queue.JobRef{ID: res.Job.ID}, nil
}

// Cancel cancels the referenced river job in the bound transaction.
func (e txEnqueuer) Cancel(ctx context.Context, ref queue.JobRef) error {
	_, err := e.client.JobCancelTx(ctx, e.tx, ref.ID)
	return err
}
