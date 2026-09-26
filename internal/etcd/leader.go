// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"context"
	"log/slog"
	"os"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/concurrency"
)

const (
	leaderRetryDelay    = time.Second
	leaderResignTimeout = 5 * time.Second
)

// fence identifies the leader key of the term that owns a context.
type fence struct {
	key string
	rev int64
}

type fenceCtxKey struct{}

// WithFence returns ctx carrying the leader key and its create revision. A
// write that adds FenceCmps(ctx) to its txn commits only while that key still
// exists with that revision, i.e. while the term that set it still leads.
func WithFence(ctx context.Context, key string, rev int64) context.Context {
	return context.WithValue(ctx, fenceCtxKey{}, fence{key: key, rev: rev})
}

// FenceCmps returns the leader-fence compare carried by ctx, or nil when ctx
// has no fence (every HTTP handler path).
func FenceCmps(ctx context.Context) []clientv3.Cmp {
	f, ok := ctx.Value(fenceCtxKey{}).(fence)
	if !ok {
		return nil
	}
	return []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(f.key), "=", f.rev)}
}

// RunAsLeader campaigns for name and runs fn while this process leads. fn's
// ctx carries the leader fence and is cancelled when the session is lost or
// ctx ends; RunAsLeader then waits for fn, resigns and campaigns again. It
// returns once ctx is done.
func RunAsLeader(ctx context.Context, c *Client, name string, ttlSeconds int, log *slog.Logger, fn func(ctx context.Context)) {
	id, _ := os.Hostname()
	for ctx.Err() == nil {
		if err := leaderTerm(ctx, c, name, ttlSeconds, id, log, fn); err != nil && ctx.Err() == nil {
			log.Warn("leader election failed; retrying", "election", name, "error", err)
			select {
			case <-ctx.Done():
			case <-time.After(leaderRetryDelay):
			}
		}
	}
}

// leaderTerm runs one campaign on its own session. Closing the session on every
// exit revokes its lease, which also deletes a key Campaign wrote but did not
// clean up after an error - a leaked session would keep that key at the head of
// the queue and stall every replica's campaign.
func leaderTerm(ctx context.Context, c *Client, name string, ttlSeconds int, id string, log *slog.Logger, fn func(context.Context)) error {
	s, err := concurrency.NewSession(c.Raw(), concurrency.WithTTL(ttlSeconds))
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }() // a failed revoke leaves the lease to expire by TTL

	tctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.Done():
			cancel()
		case <-tctx.Done():
		}
	}()

	e := concurrency.NewElection(s, Key("election", name))
	if err := e.Campaign(tctx, id); err != nil {
		return err
	}
	log.Info("became leader", "election", name, "leader", id)
	fn(WithFence(tctx, e.Key(), e.Rev()))
	log.Info("leadership ended", "election", name, "leader", id)

	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), leaderResignTimeout)
	defer rcancel()
	if err := e.Resign(rctx); err != nil {
		log.Warn("resign failed; the lease expiry releases leadership", "election", name, "error", err)
	}
	return nil
}
