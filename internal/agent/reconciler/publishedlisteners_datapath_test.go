// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package reconciler

import (
	"context"
	"io"
	"math/rand/v2"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/heartbeat"
)

func TestSelectBackendEmpty(t *testing.T) {
	got, ok := selectBackend(nil, func(int) int { t.Fatalf("rnd must not be called on an empty set"); return 0 })
	if ok {
		t.Errorf("selectBackend(nil) ok = true, want false")
	}
	if got != (heartbeat.DeclaredBackend{}) {
		t.Errorf("selectBackend(nil) backend = %+v, want zero value", got)
	}
}

func TestSelectBackendPicksIndex(t *testing.T) {
	backends := []heartbeat.DeclaredBackend{
		{OverlayIP: "10.0.0.1"},
		{OverlayIP: "10.0.0.2"},
		{OverlayIP: "10.0.0.3"},
	}
	got, ok := selectBackend(backends, func(int) int { return 1 })
	if !ok {
		t.Fatalf("selectBackend(3 backends) ok = false, want true")
	}
	if got != backends[1] {
		t.Errorf("selectBackend(3 backends, rnd->1) = %+v, want %+v", got, backends[1])
	}
}

func TestSelectBackendBalances(t *testing.T) {
	backends := []heartbeat.DeclaredBackend{
		{VMID: uuid.New(), OverlayIP: "10.0.0.1"},
		{VMID: uuid.New(), OverlayIP: "10.0.0.2"},
		{VMID: uuid.New(), OverlayIP: "10.0.0.3"},
	}
	seen := make(map[uuid.UUID]bool, len(backends))
	for range 1000 {
		got, ok := selectBackend(backends, rand.IntN)
		if !ok {
			t.Fatalf("selectBackend ok = false, want true")
		}
		seen[got.VMID] = true
	}
	for _, b := range backends {
		if !seen[b.VMID] {
			t.Errorf("backend %v never selected over 1000 calls", b.VMID)
		}
	}
}

func TestSpliceConnsCopiesBothDirections(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	// spliceConns bridges a2<->b1; a1 and b2 are the external endpoints.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go spliceConns(ctx, cancel, a2, b1)

	// a1 -> a2 -> b1 -> b2
	go func() { _, _ = a1.Write([]byte("ping")) }()
	if got := readN(t, b2, 4); got != "ping" {
		t.Errorf("a1->b2 = %q, want %q", got, "ping")
	}

	// b2 -> b1 -> a2 -> a1
	go func() { _, _ = b2.Write([]byte("pong")) }()
	if got := readN(t, a1, 4); got != "pong" {
		t.Errorf("b2->a1 = %q, want %q", got, "pong")
	}
}

func TestSpliceConnsCloseTearsDownBoth(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go spliceConns(ctx, cancel, a2, b1)

	// Closing one external endpoint makes the copy from it return, which must
	// tear down both spliced legs so a read on the other external endpoint errors.
	_ = a1.Close()

	_ = b2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := b2.Read(make([]byte, 1)); err == nil {
		t.Errorf("read on the far endpoint after close = nil error, want an error (both legs torn down)")
	}
}

// readN reads exactly n bytes from c under a short deadline and returns them as
// a string, failing the test on any read error.
func readN(t *testing.T, c net.Conn, n int) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("readN(%d): %v", n, err)
	}
	return string(buf)
}

func TestSlotLimiterPerKeyCap(t *testing.T) {
	s := newSlotLimiter(2, 10)
	for i := range 2 {
		if !s.acquire("a") {
			t.Fatalf("acquire(\"a\") #%d = false, want true", i+1)
		}
	}
	if s.acquire("a") {
		t.Errorf("third acquire(\"a\") = true, want false (per-key cap reached)")
	}
	// A different key is unaffected by the first key's cap.
	if !s.acquire("b") {
		t.Errorf("acquire(\"b\") = false, want true (independent per-key count)")
	}
}

func TestSlotLimiterTotalCap(t *testing.T) {
	s := newSlotLimiter(10, 2)
	for _, k := range []string{"a", "b"} {
		if !s.acquire(k) {
			t.Fatalf("acquire(%q) under totalCap=2 = false, want true", k)
		}
	}
	if s.acquire("c") {
		t.Errorf("acquire(\"c\") at totalCap = true, want false")
	}
}

func TestSlotLimiterDefaultCaps(t *testing.T) {
	// The raw published-port datapath constructs its accountant with these
	// package caps; keep them wired so the datapath's concurrency ceiling is a
	// single source of truth.
	s := newSlotLimiter(publishedPerBackendCap, publishedGatewayCap)
	for i := range publishedPerBackendCap {
		if !s.acquire("a") {
			t.Fatalf("acquire(\"a\") #%d under publishedPerBackendCap=%d = false, want true", i+1, publishedPerBackendCap)
		}
	}
	if s.acquire("a") {
		t.Errorf("acquire(\"a\") past publishedPerBackendCap=%d = true, want false", publishedPerBackendCap)
	}
}

func TestSlotLimiterReleaseFreesSlot(t *testing.T) {
	s := newSlotLimiter(1, 1)
	if !s.acquire("a") {
		t.Fatalf("first acquire(\"a\") = false, want true")
	}
	if s.acquire("a") {
		t.Fatalf("second acquire(\"a\") = true, want false")
	}
	s.release("a")
	if !s.acquire("a") {
		t.Errorf("acquire(\"a\") after release = false, want true (slot freed)")
	}
}
