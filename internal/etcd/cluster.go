// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// Membership-change orchestration primitives for the single->HA transition and
// HA expand/shrink. These are the client-side half: they operate against an
// existing member's client URL via a short-lived network client. The local half
// (starting the joining member's own embedded etcd in ModeJoin) is wired into
// the operator-driven transition flow, which also needs the CA-before-etcd
// provisioning (slice 9d+).
//
// etcd serializes reconfiguration and settle-gates it: its strict reconfig check
// (isConnectedFullySince) rejects a membership change with "unhealthy cluster"
// until the leader has been continuously connected to every peer for ~one
// election timeout. Right after a prior change that connection has not matured,
// so adds/removes retry until the cluster settles - the operator rule of "one
// member at a time, let it settle" encoded as a loop.
//
// AddLearner, PromoteMember, WaitMemberServing, and VoterCount are the low-level
// transition primitives exercised directly by the single->HA transition
// integration test. The production control-plane path does not call them: it goes
// through MembershipClient (membership.go), whose RegisterLearner is the
// idempotent, context-aware learner-add used by the cluster-join handler and
// whose TryPromoteLearners is the single-shot promote used by the periodic promote
// loop. They are kept distinct on purpose - the test validates the etcd-layer
// mechanics without the CP stack, while MembershipClient adds the handler-facing
// semantics.
const (
	reconfigSettleTimeout = 20 * time.Second
	reconfigRetryInterval = 500 * time.Millisecond

	// Promotion retries until the learner's raft log has caught up; etcd
	// rejects it until then, so the loop IS the catch-up wait.
	promoteTimeout       = 30 * time.Second
	promoteRetryInterval = 100 * time.Millisecond

	// A freshly promoted voter applies the config change locally a moment after
	// the leader accepts it; until then it rejects reads. Poll until it serves.
	memberServingTimeout = 15 * time.Second
	memberServingPoll    = 100 * time.Millisecond

	memberDialTimeout = 5 * time.Second
)

// dialMember opens a short-lived client to a single member's client endpoint.
// The caller closes it.
func dialMember(clientURL string) (*clientv3.Client, error) {
	return clientv3.New(clientv3.Config{
		Endpoints:   []string{clientURL},
		DialTimeout: memberDialTimeout,
	})
}

// AddLearner adds a non-voting learner at peerURL to the cluster reached via
// clientURL and returns the new member id. A learner does not count toward the
// voter majority, so quorum is never reduced by the add. Settle-gated: retries
// while etcd reports a transient reconfig rejection, until reconfigSettleTimeout.
func AddLearner(ctx context.Context, clientURL, peerURL string, log *slog.Logger) (uint64, error) {
	cli, err := dialMember(clientURL)
	if err != nil {
		return 0, fmt.Errorf("connect member: %v", err)
	}
	defer func() { _ = cli.Close() }()

	deadline := time.Now().Add(reconfigSettleTimeout)
	attempt := 0
	for {
		attempt++
		actx, acancel := context.WithTimeout(ctx, 3*time.Second)
		resp, err := cli.MemberAddAsLearner(actx, []string{peerURL})
		acancel()
		if err == nil {
			log.InfoContext(ctx, "learner added",
				slog.String("member_id", fmt.Sprintf("%x", resp.Member.ID)),
				slog.String("peer_url", peerURL))
			return resp.Member.ID, nil
		}
		if !isTransientReconfigErr(err) || time.Now().After(deadline) {
			return 0, fmt.Errorf("member add as learner after %d attempts: %v", attempt, err)
		}
		log.InfoContext(ctx, "member add retrying (cluster settling)",
			slog.Int("attempt", attempt), slog.String("error", err.Error()))
		time.Sleep(reconfigRetryInterval)
	}
}

// PromoteMember promotes a learner to a voting member, retrying until etcd
// accepts it. etcd rejects promotion until the learner's raft log has caught up,
// so the retry loop is the catch-up wait. On success it waits until the new
// voter actually serves reads (it applies the promotion locally a moment after
// the leader accepts it).
func PromoteMember(ctx context.Context, clientURL string, memberID uint64, log *slog.Logger) error {
	cli, err := dialMember(clientURL)
	if err != nil {
		return fmt.Errorf("connect member: %v", err)
	}
	defer func() { _ = cli.Close() }()

	start := time.Now()
	attempt := 0
	var promoteErr error
	for deadline := time.Now().Add(promoteTimeout); time.Now().Before(deadline); {
		// Honor cancellation promptly: without this the loop would busy-retry
		// for the full promoteTimeout on a cancelled ctx (e.g. CP shutdown).
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("promote member %x cancelled: %v", memberID, err)
		}
		attempt++
		pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
		_, promoteErr = cli.MemberPromote(pctx, memberID)
		pcancel()
		if promoteErr == nil {
			break
		}
		time.Sleep(promoteRetryInterval)
	}
	if promoteErr != nil {
		return fmt.Errorf("promote member %x after %d attempts: %v", memberID, attempt, promoteErr)
	}
	log.InfoContext(ctx, "learner promoted to voter",
		slog.String("member_id", fmt.Sprintf("%x", memberID)),
		slog.Duration("catch_up", time.Since(start)),
		slog.Int("promote_attempts", attempt))
	return nil
}

// RemoveMember removes a member from the cluster reached via clientURL. The
// caller stops the removed member's process afterward. Settle-gated like the add.
func RemoveMember(ctx context.Context, clientURL string, memberID uint64, log *slog.Logger) error {
	cli, err := dialMember(clientURL)
	if err != nil {
		return fmt.Errorf("connect member: %v", err)
	}
	defer func() { _ = cli.Close() }()

	deadline := time.Now().Add(reconfigSettleTimeout)
	attempt := 0
	for {
		attempt++
		rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := cli.MemberRemove(rctx, memberID)
		rcancel()
		if err == nil {
			log.InfoContext(ctx, "member removed", slog.String("member_id", fmt.Sprintf("%x", memberID)))
			return nil
		}
		if !isTransientReconfigErr(err) || time.Now().After(deadline) {
			return fmt.Errorf("member remove %x after %d attempts: %v", memberID, attempt, err)
		}
		log.InfoContext(ctx, "member remove retrying (cluster settling)",
			slog.Int("attempt", attempt), slog.String("error", err.Error()))
		time.Sleep(reconfigRetryInterval)
	}
}

// WaitMemberServing polls the member at clientURL until a read succeeds,
// confirming it has applied its promotion and serves as a voter (a learner or
// not-yet-applied voter rejects range requests with "rpc not supported for
// learner").
func WaitMemberServing(ctx context.Context, clientURL string) error {
	cli, err := dialMember(clientURL)
	if err != nil {
		return fmt.Errorf("connect member: %v", err)
	}
	defer func() { _ = cli.Close() }()

	deadline := time.Now().Add(memberServingTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		gctx, gcancel := context.WithTimeout(ctx, 1*time.Second)
		_, lastErr = cli.Get(gctx, KeyPrefix+"probe/serving")
		gcancel()
		if lastErr == nil {
			return nil
		}
		time.Sleep(memberServingPoll)
	}
	return fmt.Errorf("member not serving within deadline: %v", lastErr)
}

// VoterCount returns the number of voting (non-learner) members the cluster at
// clientURL reports. Operators check this to confirm a transition never rests at
// 2 voters (worst-case quorum).
func VoterCount(ctx context.Context, clientURL string) (int, error) {
	cli, err := dialMember(clientURL)
	if err != nil {
		return 0, fmt.Errorf("connect member: %v", err)
	}
	defer func() { _ = cli.Close() }()

	resp, err := cli.MemberList(ctx)
	if err != nil {
		return 0, fmt.Errorf("member list: %v", err)
	}
	n := 0
	for _, m := range resp.Members {
		if !m.IsLearner {
			n++
		}
	}
	return n, nil
}

// isTransientReconfigErr reports whether err is one of etcd's settling-related
// reconfiguration rejections that clears with time (as opposed to a permanent
// failure like an unknown member).
func isTransientReconfigErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "unhealthy cluster") ||
		strings.Contains(msg, "too many learner") ||
		strings.Contains(msg, "not enough started members")
}
