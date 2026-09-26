// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcd_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// termRecorder counts running terms and remembers the fence of the latest one.
type termRecorder struct {
	mu      sync.Mutex
	running int
	maxSeen int
	started chan context.Context
}

func newTermRecorder() *termRecorder { return &termRecorder{started: make(chan context.Context, 8)} }

func (r *termRecorder) fn(ctx context.Context) {
	r.mu.Lock()
	r.running++
	if r.running > r.maxSeen {
		r.maxSeen = r.running
	}
	r.mu.Unlock()
	r.started <- ctx
	<-ctx.Done()
	r.mu.Lock()
	r.running--
	r.mu.Unlock()
}

func waitTerm(t *testing.T, r *termRecorder) context.Context {
	t.Helper()
	select {
	case ctx := <-r.started:
		return ctx
	case <-time.After(20 * time.Second):
		t.Fatal("no term started within 20s")
		return nil
	}
}

func TestRunAsLeaderOneTermAtATime(t *testing.T) {
	cli := startTestClient(t)
	rec := newTermRecorder()
	ctxA, cancelA := context.WithCancel(context.Background())
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	doneA := make(chan struct{})
	go func() { etcd.RunAsLeader(ctxA, cli, "t1", 5, discardLog(), rec.fn); close(doneA) }()
	// Start B only once A leads, so the leader cancelled below is known.
	first := waitTerm(t, rec)
	go etcd.RunAsLeader(ctxB, cli, "t1", 5, discardLog(), rec.fn)
	select {
	case <-rec.started:
		t.Fatal("second term started while the first still leads")
	case <-time.After(2 * time.Second):
	}
	// Parent cancel of the leader resigns; the other loop takes over at once
	// (well under the 5s TTL).
	cancelA()
	<-first.Done()
	<-doneA
	start := time.Now()
	waitTerm(t, rec)
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("takeover after resign took %v, want < 4s (resign should not wait for TTL)", d)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.maxSeen != 1 {
		t.Errorf("max concurrent terms = %d, want 1", rec.maxSeen)
	}
}

func TestRunAsLeaderLeaseLossEndsTerm(t *testing.T) {
	cli := startTestClient(t)
	rec := newTermRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go etcd.RunAsLeader(ctx, cli, "t2", 5, discardLog(), rec.fn)
	term := waitTerm(t, rec)

	// Find the leader key through the fence and revoke its lease from outside,
	// modelling a lease that expired while the process stalled.
	cmps := etcd.FenceCmps(term)
	if len(cmps) != 1 {
		t.Fatalf("FenceCmps(term) = %d cmps, want 1", len(cmps))
	}
	resp, err := cli.Raw().Get(ctx, etcd.Key("election", "t2"), clientv3.WithPrefix())
	if err != nil || len(resp.Kvs) != 1 {
		t.Fatalf("Get election keys = %v, %v; want 1 key", resp, err)
	}
	if _, err := cli.Raw().Revoke(ctx, clientv3.LeaseID(resp.Kvs[0].Lease)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	select {
	case <-term.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("term ctx not cancelled after the leader's lease was revoked")
	}
	// The loop campaigns again on a fresh session and leads again.
	waitTerm(t, rec)
}

func TestRunAsLeaderLeavesNoKeyAfterAbandonedCampaign(t *testing.T) {
	cli := startTestClient(t)
	rec := newTermRecorder()
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	go etcd.RunAsLeader(ctxA, cli, "t3", 5, discardLog(), rec.fn)
	waitTerm(t, rec)

	// B campaigns behind A (its key is written), then its parent is cancelled
	// mid-campaign. When B's RunAsLeader returns, only A's key may remain.
	prefix := etcd.Key("election", "t3") + "/"
	ctxB, cancelB := context.WithCancel(context.Background())
	doneB := make(chan struct{})
	go func() { etcd.RunAsLeader(ctxB, cli, "t3", 5, discardLog(), rec.fn); close(doneB) }()
	var leaseB clientv3.LeaseID
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := cli.Raw().Get(context.Background(), prefix, clientv3.WithPrefix())
		if err == nil && len(resp.Kvs) == 2 {
			// B's key is the later of the two campaign keys.
			b := resp.Kvs[0]
			if resp.Kvs[1].CreateRevision > b.CreateRevision {
				b = resp.Kvs[1]
			}
			leaseB = clientv3.LeaseID(b.Lease)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("B never wrote its campaign key")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancelB()
	<-doneB
	resp, err := cli.Raw().Get(context.Background(), prefix, clientv3.WithPrefix())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(resp.Kvs) != 1 {
		t.Errorf("election keys after B returned = %d, want 1 (B's key must be gone)", len(resp.Kvs))
	}
	// Campaign deletes its own key on a parent cancel, so the key count alone
	// cannot tell whether B's session was closed; its lease must be revoked.
	ttl, err := cli.Raw().TimeToLive(context.Background(), leaseB)
	if err != nil {
		t.Fatalf("TimeToLive(%x): %v", leaseB, err)
	}
	if ttl.TTL != -1 {
		t.Errorf("TimeToLive(B's lease).TTL = %d, want -1 (B's session lease must be revoked)", ttl.TTL)
	}
}

func TestFenceCmpsWithoutFence(t *testing.T) {
	if got := etcd.FenceCmps(context.Background()); got != nil {
		t.Errorf("FenceCmps(no fence) = %v, want nil", got)
	}
}
