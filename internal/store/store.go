// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package store is the api-server's data access layer. It owns a pgx
// connection pool, exposes the sqlc-generated Querier as Queries, and
// supports transactional execution via InTx.
//
// The generated files (db.go, models.go, querier.go, *.sql.go) come from
// sqlc and are not edited by hand; regenerate with `make sqlc-generate`.
package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/otherix/otherix/internal/config"
)

// Store wraps a pgx pool and the sqlc-generated Queries. Construct with
// NewStore; close with Close. The queueBinder is wired post-construction
// via SetQueueBinder (the queue client is built after the store).
type Store struct {
	pool        *pgxpool.Pool
	queries     *Queries
	queueBinder QueueBinder
}

// NewStore opens a pgx pool against cfg.DSN, applies pool-sizing
// parameters, pings the database to verify connectivity, and returns a
// ready-to-use Store. Caller must call Close.
func NewStore(ctx context.Context, cfg config.DatabaseConfig) (*Store, error) {
	if cfg.DSN == "" {
		return nil, errors.New("database dsn is required")
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if cfg.MaxConns > 0 {
		if cfg.MaxConns > math.MaxInt32 {
			return nil, fmt.Errorf("max_conns out of range: %d", cfg.MaxConns)
		}
		poolCfg.MaxConns = int32(cfg.MaxConns)
	}
	if cfg.MinConns > 0 {
		if cfg.MinConns > math.MaxInt32 {
			return nil, fmt.Errorf("min_conns out of range: %d", cfg.MinConns)
		}
		poolCfg.MinConns = int32(cfg.MinConns)
	}
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	// Connectivity check on construction. The 15s budget is generous
	// for a healthy DB (real ping under load completes in tens of ms)
	// and tolerates testcontainer Docker daemon hiccups when many
	// pools open in quick succession across a parallel test suite.
	// True unreachable-DB cases fail at the TCP layer well before
	// this deadline, so widening it does not paper over real outages.
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	return &Store{
		pool:    pool,
		queries: New(pool),
	}, nil
}

// FromPool wraps an existing pgx pool into a *Store. Production code
// should use NewStore (which validates connectivity through Ping);
// FromPool exists so integration tests that already hold a pool from
// a fixture (e.g. internal/migrationtest.Harness) can construct a
// Store over it without opening a second connection.
func FromPool(pool *pgxpool.Pool) *Store {
	return &Store{
		pool:    pool,
		queries: New(pool),
	}
}

// Queries returns the Queries instance bound to the pool, for queries
// outside a transaction. Inside a transaction, use the Queries argument
// passed to InTx instead.
func (s *Store) Queries() *Queries {
	return s.queries
}

// Pool returns the underlying pgx pool. Use sparingly — most callers
// should go through Queries or InTx. The pool is needed for raw access
// such as LISTEN/NOTIFY or applying schema migrations at startup.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

// InTx runs fn inside a database transaction. If fn returns an error or
// panics, the transaction is rolled back; otherwise it is committed. The
// Queries passed to fn is bound to the transaction — use it instead of
// Store.Queries inside fn.
func (s *Store) InTx(ctx context.Context, fn func(*Queries) error) (err error) {
	return s.InTxWithTx(ctx, func(q *Queries, _ pgx.Tx) error { return fn(q) })
}

// InTxWithTx is the cross-component variant of InTx: it exposes the
// underlying pgx.Tx alongside the queries-bound *Queries so callers
// that need to layer non-store work into the same transaction (e.g.
// `riverClient.JobCancelTx` / `riverClient.InsertTx`) can do so
// atomically. Use InTx in pure-store paths; reach for this helper
// only when the transaction has to span the store/river boundary.
func (s *Store) InTxWithTx(ctx context.Context, fn func(*Queries, pgx.Tx) error) (err error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// Always attempt rollback. After a successful Commit, Rollback is a
	// no-op (pgx documents this), so this is safe.
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			// Don't shadow a real error; only surface rollback failure
			// when the original path was clean.
			if err == nil {
				err = fmt.Errorf("rollback: %w", rbErr)
			}
		}
	}()

	if err = fn(s.queries.WithTx(tx), tx); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Close closes the underlying pool. Idempotent.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}
