// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"io"
	"net"
	"sync"

	"github.com/otherix/otherix/internal/agent/heartbeat"
)

// Connection-slot caps for the raw published-port datapath. They bound the
// number of concurrent spliced sessions a single gateway carries, per selected
// backend and in total, so one backend cannot exhaust the gateway's file
// descriptors or memory and a gateway has a hard ceiling on fan-out. The per-key
// is the selected backend VMID.
const (
	publishedPerBackendCap = 8
	publishedGatewayCap    = 256
)

// selectBackend picks a uniformly random backend from the CP-pushed set and
// returns it with true, or the zero value and false when the set is empty.
// rnd(n) must return a value in [0,n); production callers pass math/rand/v2's
// IntN, tests pass a deterministic stub.
//
// The set is selected in full: the CP already applied backend eligibility
// (fail toward inclusion) before pushing it, so DeclaredBackend.Healthy is
// informational and must not be re-filtered here - doing so would wrongly
// darken a warming-but-eligible backend.
func selectBackend(backends []heartbeat.DeclaredBackend, rnd func(int) int) (heartbeat.DeclaredBackend, bool) {
	if len(backends) == 0 {
		return heartbeat.DeclaredBackend{}, false
	}
	return backends[rnd(len(backends))], true
}

// spliceConns copies bytes both directions until either side closes or ctx is
// cancelled, then tears both legs down (the kill-implies-teardown invariant: no
// goroutine, fd, or slot survives any exit path). All copy and close errors are
// discarded - the only outcome that matters is that both connections end closed.
//
// This is a deliberate reimplementation of the sibling ingress splicer,
// internal/agent/ingress/splice.go (spliceConns), kept local to this datapath
// rather than shared: the /v1/connect path is load-bearing and reliability
// beats the trivial DRY saving.
func spliceConns(ctx context.Context, cancel context.CancelFunc, a, b net.Conn) {
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)

	select {
	case <-ctx.Done():
	case <-done:
	}
	cancel()
	_ = a.Close()
	_ = b.Close()
}

// slotLimiter enforces the per-backend and per-gateway concurrency caps on the
// raw published-port splice plane, mirroring the sibling ingress accountant,
// internal/agent/ingress/splice.go (connectSlots).
type slotLimiter struct {
	mu        sync.Mutex
	perKey    map[string]int
	total     int
	perKeyCap int
	totalCap  int
}

// newSlotLimiter builds a slot accountant with the given per-key and total caps.
func newSlotLimiter(perKeyCap, totalCap int) *slotLimiter {
	return &slotLimiter{
		perKey:    map[string]int{},
		perKeyCap: perKeyCap,
		totalCap:  totalCap,
	}
}

// acquire reserves a slot for key, enforcing the per-key and total caps. It
// returns true on success, or false (reserving nothing) when either cap is
// already reached.
func (s *slotLimiter) acquire(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total >= s.totalCap || s.perKey[key] >= s.perKeyCap {
		return false
	}
	s.perKey[key]++
	s.total++
	return true
}

// release returns a slot previously taken by acquire, deleting the map entry
// when its count falls to zero.
func (s *slotLimiter) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perKey[key] > 0 {
		s.perKey[key]--
		if s.perKey[key] == 0 {
			delete(s.perKey, key)
		}
		s.total--
	}
}
