// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package etcdstore implements the control-plane storage surface over embedded
// etcd (ADR 0030 Phase 3). It satisfies the same per-handler-package Store
// interfaces the SQL-backed *store.Store satisfies, reusing the model, param,
// and sentinel types from the store package so handlers stay untouched until
// cutover.
//
// Key-schema conventions (established here, reused per resource):
//
//   - Primary: /otherix/<resource>/<id> -> JSON-encoded store.<Resource>.
//   - Uniqueness guard: /otherix/uniq/<resource>/<field>/<lower(value)> -> id,
//     created with PutIfAbsent (or a compare in a multi-op txn) - replaces the
//     SQL partial unique index. Removed when the owning row is (soft-)deleted so
//     the value becomes reusable, matching `where deleted_at is null`.
//   - Secondary index (high-cardinality / owner-scoped lists): a maintained
//     key whose lexical order matches the SQL ORDER BY, written in the same txn
//     as the primary. Bounded cluster-wide collections (e.g. networks) skip the
//     index and scan the primary prefix instead.
//   - Application-layer enforcement of what the schema used to enforce:
//     constraints, uniqueness, FK-cascade blocking, and updated_at bumps all
//     move into these methods (ADR 0030 #7).
package etcdstore

import (
	"context"

	"github.com/otherix/otherix/internal/etcd"
)

// Store is the etcd-backed implementation of the handler Store interfaces. It
// holds a KV client over the embedded member; one Store instance accumulates
// every resource's methods as the Phase 3 slices land.
type Store struct {
	c *etcd.Client
}

// New constructs a Store over the given KV client.
func New(c *etcd.Client) *Store {
	return &Store{c: c}
}

// healthPingKey is a sentinel the readiness probe reads to confirm the etcd
// member answers reads. It need not exist; a successful (empty) read still
// round-trips to the member.
const healthPingKey = "/otherix/health/ping"

// Ping verifies the etcd member is reachable by issuing a linearizable read.
// It backs the api-server's /readyz probe (health.Pinger).
func (s *Store) Ping(ctx context.Context) error {
	_, _, err := s.c.Get(ctx, healthPingKey)
	return err
}
