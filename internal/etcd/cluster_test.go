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
	n, err := etcd.VoterCount(context.Background(), clientURL)
	if err != nil {
		t.Fatalf("VoterCount: %v", err)
	}
	if n != 1 {
		t.Errorf("VoterCount(single node) = %d, want 1", n)
	}
}

func TestWaitMemberServing(t *testing.T) {
	clientURL := startMemberWithClientURL(t)
	if err := etcd.WaitMemberServing(context.Background(), clientURL); err != nil {
		t.Errorf("WaitMemberServing(serving single node): %v", err)
	}
}

// TestAddLearnerThenRemove adds a learner pointing at a phantom (never-started)
// peer and removes it again. The learner registers in membership but is never a
// voter, so the single voter stays operational and VoterCount holds at 1
// throughout - the add/remove round-trip exercises the settle-gated paths
// without needing a second running member (that is slice 9e's transition test).
func TestAddLearnerThenRemove(t *testing.T) {
	clientURL := startMemberWithClientURL(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	phantomPeer := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	id, err := etcd.AddLearner(ctx, clientURL, phantomPeer, log)
	if err != nil {
		t.Fatalf("AddLearner: %v", err)
	}
	if id == 0 {
		t.Fatal("AddLearner returned member id 0")
	}

	if n, err := etcd.VoterCount(ctx, clientURL); err != nil || n != 1 {
		t.Errorf("VoterCount after add-learner = (%d, %v), want (1, nil) - learner is not a voter", n, err)
	}

	if err := etcd.RemoveMember(ctx, clientURL, id, log); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	if n, err := etcd.VoterCount(ctx, clientURL); err != nil || n != 1 {
		t.Errorf("VoterCount after remove = (%d, %v), want (1, nil)", n, err)
	}
}
