// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcd_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/otherix/otherix/internal/etcd"
)

func TestVoterCountSingleNode(t *testing.T) {
	clientURL := startMemberWithClientURL(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mc := etcd.NewMembershipClient(clientURL, log)
	n, err := mc.VoterCount(context.Background())
	if err != nil {
		t.Fatalf("VoterCount: %v", err)
	}
	if n != 1 {
		t.Errorf("VoterCount(single node) = %d, want 1", n)
	}
}

func TestWaitServing(t *testing.T) {
	clientURL := startMemberWithClientURL(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mc := etcd.NewMembershipClient(clientURL, log)
	if err := mc.WaitServing(context.Background()); err != nil {
		t.Errorf("WaitServing(serving single node): %v", err)
	}
}

// TestAddLearnerThenRemove adds a learner pointing at a phantom (never-started)
// peer and removes it again. The learner registers in membership but is never a
// voter, so the single voter stays operational and VoterCount holds at 1
// throughout - the add/remove round-trip exercises the settle-gated paths
// without needing a second running member (the transition is a separate test).
func TestAddLearnerThenRemove(t *testing.T) {
	clientURL := startMemberWithClientURL(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mc := etcd.NewMembershipClient(clientURL, log)
	ctx := context.Background()

	phantomPeer := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	members, err := mc.RegisterLearner(ctx, phantomPeer)
	if err != nil {
		t.Fatalf("RegisterLearner: %v", err)
	}
	learnerID := learnerMemberID(t, members, phantomPeer)

	if n, err := mc.VoterCount(ctx); err != nil || n != 1 {
		t.Errorf("VoterCount after add-learner = (%d, %v), want (1, nil) - learner is not a voter", n, err)
	}

	if err := mc.RemoveMember(ctx, learnerID); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	if n, err := mc.VoterCount(ctx); err != nil || n != 1 {
		t.Errorf("VoterCount after remove = (%d, %v), want (1, nil)", n, err)
	}
}

// learnerMemberID returns the member id of the freshly registered learner: the
// entry flagged IsLearner whose peer URLs contain peerURL.
func learnerMemberID(t *testing.T, members []etcd.Member, peerURL string) uint64 {
	t.Helper()
	for _, m := range members {
		if !m.IsLearner {
			continue
		}
		for _, u := range m.PeerURLs {
			if u == peerURL {
				return m.ID
			}
		}
	}
	t.Fatalf("learner with peer URL %q not found in members %+v", peerURL, members)
	return 0
}
