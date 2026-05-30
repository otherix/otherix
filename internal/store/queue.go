// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/queue"
)

// QueueBinder produces a queue.Enqueuer bound to a transaction. It is
// the seam between the store's transactional substrate (a pgx.Tx in the
// Postgres era) and the queue backend. Injected after construction via
// SetQueueBinder because the queue client is built after the store.
type QueueBinder interface {
	Bind(tx pgx.Tx) queue.Enqueuer
}

// SetQueueBinder wires the queue backend into the store. Called once at
// startup after the queue client exists.
func (s *Store) SetQueueBinder(b QueueBinder) { s.queueBinder = b }

// InTxEnqueue runs fn inside a transaction, passing it the queries and a
// queue.Enqueuer bound to the same transaction so a task row and its
// background job commit atomically. It returns an error without opening
// a transaction when no queue backend has been configured. This is the
// backend-agnostic replacement for handlers reaching for InTxWithTx +
// the raw river client.
func (s *Store) InTxEnqueue(ctx context.Context, fn func(*Queries, queue.Enqueuer) error) error {
	if s.queueBinder == nil {
		return errors.New("store: queue binder not configured")
	}
	return s.InTxWithTx(ctx, func(q *Queries, tx pgx.Tx) error {
		return fn(q, s.queueBinder.Bind(tx))
	})
}
