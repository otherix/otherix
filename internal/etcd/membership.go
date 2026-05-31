// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Member is a cluster membership entry as reported by etcd. Name is empty for a
// freshly added learner that has not yet started and registered.
type Member struct {
	ID         uint64
	Name       string
	PeerURLs   []string
	ClientURLs []string
	IsLearner  bool
}

// MembershipClient is a loopback-bound seam for cluster membership operations.
// It dials its own member's client URL for each call, so a single instance can
// be shared across the join handler and the periodic promote worker.
type MembershipClient struct {
	clientURL string
	log       *slog.Logger
}

// NewMembershipClient constructs a MembershipClient bound to clientURL. Each
// method dials a short-lived client on demand; no persistent connection is held.
func NewMembershipClient(clientURL string, log *slog.Logger) *MembershipClient {
	return &MembershipClient{clientURL: clientURL, log: log}
}

// RegisterLearner adds peerURL as a learner and returns the resulting membership.
// Idempotent on an already-registered peer URL (etcd's "Peer URLs already exists"
// treated as success). Settle-gated: retries while isTransientReconfigErr(err) is
// true, until reconfigSettleTimeout, sleeping reconfigRetryInterval between tries.
func (m *MembershipClient) RegisterLearner(ctx context.Context, peerURL string) ([]Member, error) {
	cli, err := dialMember(m.clientURL)
	if err != nil {
		return nil, fmt.Errorf("connect member: %v", err)
	}
	defer func() { _ = cli.Close() }()

	deadline := time.Now().Add(reconfigSettleTimeout)
	attempt := 0
	for {
		// Honor cancellation promptly: a cancelled ctx (e.g. CP shutdown) must
		// not keep retrying or sleeping through the settle window.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("register learner cancelled: %v", err)
		}
		attempt++
		actx, acancel := context.WithTimeout(ctx, 3*time.Second)
		resp, addErr := cli.MemberAddAsLearner(actx, []string{peerURL})
		acancel()
		if addErr == nil {
			m.log.InfoContext(ctx, "learner registered",
				slog.String("member_id", fmt.Sprintf("%x", resp.Member.ID)),
				slog.String("peer_url", peerURL))
			break
		}
		if isPeerURLExistErr(addErr) {
			m.log.InfoContext(ctx, "learner already registered (idempotent)",
				slog.String("peer_url", peerURL))
			break
		}
		if !isTransientReconfigErr(addErr) || time.Now().After(deadline) {
			return nil, fmt.Errorf("register learner after %d attempts: %v", attempt, addErr)
		}
		m.log.InfoContext(ctx, "register learner retrying (cluster settling)",
			slog.Int("attempt", attempt), slog.String("error", addErr.Error()))
		time.Sleep(reconfigRetryInterval)
	}
	return membersFrom(ctx, cli)
}

// ListMembers returns the cluster membership reported by the local member.
func (m *MembershipClient) ListMembers(ctx context.Context) ([]Member, error) {
	cli, err := dialMember(m.clientURL)
	if err != nil {
		return nil, fmt.Errorf("connect member: %v", err)
	}
	defer func() { _ = cli.Close() }()

	return membersFrom(ctx, cli)
}

// RemoveMember removes the member with the given id. The caller stops the
// removed member's process afterward. Settle-gated like RegisterLearner: retries
// while isTransientReconfigErr(err) is true, until reconfigSettleTimeout, sleeping
// reconfigRetryInterval between tries.
func (m *MembershipClient) RemoveMember(ctx context.Context, id uint64) error {
	cli, err := dialMember(m.clientURL)
	if err != nil {
		return fmt.Errorf("connect member: %v", err)
	}
	defer func() { _ = cli.Close() }()

	deadline := time.Now().Add(reconfigSettleTimeout)
	attempt := 0
	for {
		// Honor cancellation promptly: a cancelled ctx (e.g. CP shutdown) must
		// not keep retrying or sleeping through the settle window.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("remove member %x cancelled: %v", id, err)
		}
		attempt++
		rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
		_, rmErr := cli.MemberRemove(rctx, id)
		rcancel()
		if rmErr == nil {
			m.log.InfoContext(ctx, "member removed", slog.String("member_id", fmt.Sprintf("%x", id)))
			return nil
		}
		if !isTransientReconfigErr(rmErr) || time.Now().After(deadline) {
			return fmt.Errorf("member remove %x after %d attempts: %v", id, attempt, rmErr)
		}
		m.log.InfoContext(ctx, "member remove retrying (cluster settling)",
			slog.Int("attempt", attempt), slog.String("error", rmErr.Error()))
		time.Sleep(reconfigRetryInterval)
	}
}

// VoterCount returns the number of voting (non-learner) members the cluster
// reports through the bound member. A transition watcher polls this to confirm a
// learner has been promoted.
func (m *MembershipClient) VoterCount(ctx context.Context) (int, error) {
	members, err := m.ListMembers(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, mem := range members {
		if !mem.IsLearner {
			n++
		}
	}
	return n, nil
}

// WaitServing polls the bound member until a read succeeds, confirming it has
// applied its membership change and serves as a voter (a learner or a not-yet-
// applied voter rejects range requests). It returns an error if the member does
// not begin serving within memberServingTimeout.
func (m *MembershipClient) WaitServing(ctx context.Context) error {
	cli, err := dialMember(m.clientURL)
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

// TryPromoteLearners promotes every caught-up learner in one pass: a single
// MemberPromote attempt per learner, no waiting. etcd rejects a learner that has
// not caught up, so those are skipped (debug-logged) and retried next call.
// Best-effort: returns the count promoted and a non-nil error only when the member
// list cannot be read. The single->HA transition watcher calls this in a loop,
// retrying the single-shot promote until VoterCount reaches the target.
func (m *MembershipClient) TryPromoteLearners(ctx context.Context) (int, error) {
	cli, err := dialMember(m.clientURL)
	if err != nil {
		return 0, fmt.Errorf("connect member: %v", err)
	}
	defer func() { _ = cli.Close() }()

	members, err := membersFrom(ctx, cli)
	if err != nil {
		return 0, err
	}

	promoted := 0
	for _, mem := range members {
		if !mem.IsLearner {
			continue
		}
		pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
		_, promErr := cli.MemberPromote(pctx, mem.ID)
		pcancel()
		if promErr != nil {
			m.log.DebugContext(ctx, "learner not yet caught up; will retry",
				slog.String("member_id", fmt.Sprintf("%x", mem.ID)),
				slog.String("error", promErr.Error()))
			continue
		}
		m.log.InfoContext(ctx, "learner promoted to voter",
			slog.String("member_id", fmt.Sprintf("%x", mem.ID)))
		promoted++
	}
	return promoted, nil
}

// membersFrom fetches the current membership from an already-dialed client.
func membersFrom(ctx context.Context, cli *clientv3.Client) ([]Member, error) {
	resp, err := cli.MemberList(ctx)
	if err != nil {
		return nil, fmt.Errorf("member list: %v", err)
	}
	out := make([]Member, 0, len(resp.Members))
	for _, mm := range resp.Members {
		out = append(out, Member{
			ID:         mm.ID,
			Name:       mm.Name,
			PeerURLs:   mm.PeerURLs,
			ClientURLs: mm.ClientURLs,
			IsLearner:  mm.IsLearner,
		})
	}
	return out, nil
}

// isPeerURLExistErr reports whether err is etcd's rejection of an add whose peer
// URL is already a member - the idempotency signal for RegisterLearner. etcd may
// return "Peer URLs already exists" or "member ID already exist" depending on
// whether the URL or the derived member ID collides first. It matches the typed
// clientv3 sentinels first (robust against message rewording) and falls back to
// the message substrings for callers that hand it a plain string error.
func isPeerURLExistErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, rpctypes.ErrPeerURLExist) || errors.Is(err, rpctypes.ErrMemberExist) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "Peer URLs already exists") ||
		strings.Contains(msg, "member ID already exist")
}
