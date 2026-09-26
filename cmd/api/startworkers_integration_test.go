// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration

package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/config"
	"github.com/otherix/otherix/internal/etcd"
)

// TestStartWorkersRunsUnderElectionAndResignsOnStop drives the real
// startWorkers path: the workers must campaign for the "workers" election, and
// after ctx is cancelled the returned closure must return (term drained) with
// the election key released, so a peer replica can take over at once instead
// of waiting out the session TTL.
func TestStartWorkersRunsUnderElectionAndResignsOnStop(t *testing.T) {
	st, _, _, _ := runServeTestDeps(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := &config.APIConfig{}
	cfg.Workers.Enabled = true
	cfg.StoragePools.AllowedPathPrefixes = []string{"/var/lib/otherix/pools/"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A zero Client is enough: the queue is empty, so no handler dials an agent.
	stop, err := startWorkers(ctx, cfg, st, &agentclient.Client{}, log)
	if err != nil {
		t.Fatalf("startWorkers: %v", err)
	}

	prefix := etcd.Key("election", "workers")
	electionKeys := func() int64 {
		t.Helper()
		resp, err := st.Client().Raw().Get(context.Background(), prefix, clientv3.WithPrefix(), clientv3.WithCountOnly())
		if err != nil {
			t.Fatalf("get election keys: %v", err)
		}
		return resp.Count
	}

	deadline := time.Now().Add(10 * time.Second)
	for electionKeys() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no key under %q: startWorkers did not campaign for the workers election", prefix)
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	start := time.Now()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("stop closure did not return after ctx cancel")
	}
	t.Logf("workers drained in %v", time.Since(start))

	if n := electionKeys(); n != 0 {
		t.Errorf("election keys after stop = %d, want 0 (leadership not released)", n)
	}
}
