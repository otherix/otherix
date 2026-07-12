// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package etcdstore implements the control-plane storage surface over embedded
// etcd. It satisfies the same per-handler-package Store
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
//     move into these methods.
package etcdstore

import (
	"context"
	"log/slog"
	"time"

	"github.com/otherix/otherix/internal/etcd"
)

// Store is the etcd-backed implementation of the handler Store interfaces. It
// holds a KV client over the embedded member; one Store instance carries
// every resource's methods.
type Store struct {
	c                 *etcd.Client
	log               *slog.Logger
	placementLk       *placementLocker
	refreshTokenTTL   time.Duration
	downPathStaleness time.Duration
}

// defaultRefreshTokenTTL is the fallback lifetime the store assumes for refresh
// tokens when a constructor does not plumb the real auth config. It sizes the
// family-burn barrier's expiry (see refreshFamilyBarrierKey); a safe bound for
// the <=30d common case. Production (cmd/api/serve.go) overrides it with the
// operator-configured JWTRefreshTTL via WithRefreshTokenTTL.
const defaultRefreshTokenTTL = 31 * 24 * time.Hour

// defaultDownPathStaleness bounds how recent a NAT'd mesh node's own reported
// handshake set must be for placement to still treat that node as reachable
// through a gateway. Production overrides it from config via WithDownPathStaleness.
const defaultDownPathStaleness = 90 * time.Second

// Option configures a Store.
type Option func(*Store)

// WithLogger sets the Store's logger, used for quarantine warnings when a
// persisted key fails to decode. Defaults to slog.Default().
func WithLogger(log *slog.Logger) Option { return func(s *Store) { s.log = log } }

// WithRefreshTokenTTL tells the store how long refresh tokens live, so the
// family-burn barrier can be sized to outlive every token in a burned family.
// A non-positive value is ignored (the default stands).
func WithRefreshTokenTTL(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.refreshTokenTTL = d
		}
	}
}

// WithDownPathStaleness sets the freshness window for a NAT'd mesh node's own
// reported handshake set to keep it schedulable. A non-positive value is
// ignored (the default stands).
func WithDownPathStaleness(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.downPathStaleness = d
		}
	}
}

// New constructs a Store over the given KV client.
func New(c *etcd.Client, opts ...Option) *Store {
	s := &Store{c: c, log: slog.Default(), placementLk: newPlacementLocker(), refreshTokenTTL: defaultRefreshTokenTTL, downPathStaleness: defaultDownPathStaleness}
	for _, o := range opts {
		o(s)
	}
	return s
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
