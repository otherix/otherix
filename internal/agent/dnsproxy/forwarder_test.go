// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package dnsproxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"
)

// TestRunWaitsForInflightRelaysBeforeReturning: Run must not return until in-flight relays finish, so
// a relay goroutine cannot outlive Run and race the package-level dnsExchange seam (fixes the R2-M4
// data race). Not parallel: it mutates the seam.
func TestRunWaitsForInflightRelaysBeforeReturning(t *testing.T) {
	relayEntered := make(chan struct{})
	release := make(chan struct{})
	orig := dnsExchange
	defer func() { dnsExchange = orig }()
	dnsExchange = func(string, []byte, time.Duration) ([]byte, error) {
		close(relayEntered)
		<-release
		return nil, errors.New("released")
	}
	fwd, err := New(Config{Listen: "127.0.0.1:0", Upstreams: []string{"192.0.2.1:53"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = fwd.Run(ctx); close(runDone) }()
	select {
	case <-fwd.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("forwarder not ready")
	}
	client, err := net.Dial("udp4", fwd.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial = %v", err)
	}
	defer client.Close()
	if _, err := client.Write([]byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatalf("write = %v", err)
	}
	<-relayEntered
	cancel()
	select {
	case <-runDone:
		t.Fatal("Run returned before the in-flight relay finished")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the relay finished")
	}
}

// startStubUpstream runs a UDP server that echoes back the query bytes with the
// QR (response) bit set, standing in for a real resolver. Returns its addr.
func startStubUpstream(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("stub upstream listen = %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			resp := make([]byte, n)
			copy(resp, buf[:n])
			if n >= 3 {
				resp[2] |= 0x80 // set QR bit -> response
			}
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestForwarderRelaysQuery(t *testing.T) {
	upstream := startStubUpstream(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	fwd, err := New(Config{Listen: "127.0.0.1:0", Upstreams: []string{upstream}, Log: log})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = fwd.Run(ctx); close(runDone) }()
	t.Cleanup(func() { cancel(); <-runDone })

	select {
	case <-fwd.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("forwarder did not become ready")
	}

	client, err := net.Dial("udp4", fwd.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial forwarder = %v", err)
	}
	defer client.Close()

	// Minimal DNS query header (id=0xABCD, RD set). Body is irrelevant: the stub
	// echoes and the forwarder relays bytes verbatim.
	query := []byte{0xAB, 0xCD, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	if _, err := client.Write(query); err != nil {
		t.Fatalf("write query = %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp := make([]byte, 1500)
	n, err := client.Read(resp)
	if err != nil {
		t.Fatalf("read response = %v", err)
	}
	if n < 3 || resp[0] != 0xAB || resp[1] != 0xCD {
		t.Errorf("response id mismatch: got % x", resp[:min(n, 4)])
	}
	if resp[2]&0x80 == 0 {
		t.Errorf("response QR bit not set; got %#x", resp[2])
	}
}

// startMangledTxidUpstream runs a UDP server that echoes the query but FLIPS
// the DNS transaction id (first byte), standing in for an off-path spoofer
// racing the real reply. The forwarder must reject it.
func startMangledTxidUpstream(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("stub upstream listen = %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			resp := make([]byte, n)
			copy(resp, buf[:n])
			if n >= 3 {
				resp[2] |= 0x80 // set QR bit -> response
			}
			if n >= 1 {
				resp[0] ^= 0xFF // mangle the txid: off-path forgery
			}
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestForwarderRejectsMismatchedTxid(t *testing.T) {
	upstream := startMangledTxidUpstream(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	fwd, err := New(Config{Listen: "127.0.0.1:0", Upstreams: []string{upstream}, Log: log})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = fwd.Run(ctx); close(runDone) }()
	t.Cleanup(func() { cancel(); <-runDone })

	select {
	case <-fwd.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("forwarder did not become ready")
	}

	client, err := net.Dial("udp4", fwd.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial forwarder = %v", err)
	}
	defer client.Close()

	query := []byte{0xAB, 0xCD, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	if _, err := client.Write(query); err != nil {
		t.Fatalf("write query = %v", err)
	}
	// The forwarder must NOT relay the spoofed (mangled-txid) reply, so the
	// client read should time out.
	_ = client.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	resp := make([]byte, 1500)
	if _, err := client.Read(resp); err == nil {
		t.Fatalf("client got a forwarded response for a mangled-txid reply; want timeout")
	}
}

func TestForwarderDropsAtCapacity(t *testing.T) {
	// Black-hole upstream: reads queries and never replies, so each relay parks
	// on its upstream read until the 3s timeout, holding its sem slot.
	blackhole, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("black-hole listen = %v", err)
	}
	t.Cleanup(func() { _ = blackhole.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := blackhole.ReadFrom(buf); err != nil {
				return
			}
		}
	}()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fwd, err := New(Config{
		Listen:      "127.0.0.1:0",
		Upstreams:   []string{blackhole.LocalAddr().String()},
		Log:         log,
		MaxInFlight: 4,
	})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = fwd.Run(ctx); close(runDone) }()
	t.Cleanup(func() { cancel(); <-runDone })

	select {
	case <-fwd.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("forwarder did not become ready")
	}

	client, err := net.Dial("udp4", fwd.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial forwarder = %v", err)
	}
	defer client.Close()

	query := []byte{0xAB, 0xCD, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for i := 0; i < 200; i++ {
		if _, err := client.Write(query); err != nil {
			t.Fatalf("write query %d = %v", i, err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fwd.Dropped() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Dropped() = %d, want > 0 under flood", fwd.Dropped())
}

// TestAcquireSourceCapPerIP proves a single source holds at most MaxPerSource
// concurrent slots, a different source is independent, and balanced releases
// self-clean the inflight map.
func TestAcquireSourceCapPerIP(t *testing.T) {
	f := &Forwarder{maxPerSource: 2, inflight: map[string]int{}}
	if !f.acquireSource("10.0.0.1") {
		t.Fatal("first acquire for one source must succeed")
	}
	if !f.acquireSource("10.0.0.1") {
		t.Fatal("second acquire for one source must succeed (under cap)")
	}
	if f.acquireSource("10.0.0.1") {
		t.Error("third acquire for the same source must fail (over cap)")
	}
	if !f.acquireSource("10.0.0.2") {
		t.Error("a different source must be independent of 10.0.0.1's cap")
	}
	f.releaseSource("10.0.0.1") // 2 -> 1
	if !f.acquireSource("10.0.0.1") {
		t.Error("after a release the source is under cap again")
	}
	// Balanced releases must empty the map (self-cleaning).
	f.releaseSource("10.0.0.1")
	f.releaseSource("10.0.0.1") // 10.0.0.1 -> 0
	f.releaseSource("10.0.0.2") // 10.0.0.2 -> 0
	if len(f.inflight) != 0 {
		t.Errorf("inflight map not self-cleaned: %v", f.inflight)
	}
}

// TestForwarderPerSourceCapDropsOneFloodingSource proves that with a black-hole
// upstream (relays park) and a small MaxPerSource, a single client flooding
// queries trips the PER-SOURCE cap (SourceDropped > 0) well below the global
// 256, so one source cannot consume the global pool. Mirrors
// TestForwarderDropsAtCapacity: parking upstream, fire many queries, poll.
func TestForwarderPerSourceCapDropsOneFloodingSource(t *testing.T) {
	blackhole, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("black-hole listen = %v", err)
	}
	t.Cleanup(func() { _ = blackhole.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := blackhole.ReadFrom(buf); err != nil {
				return
			}
		}
	}()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fwd, err := New(Config{
		Listen:       "127.0.0.1:0",
		Upstreams:    []string{blackhole.LocalAddr().String()},
		Log:          log,
		MaxPerSource: 2,
	})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = fwd.Run(ctx); close(runDone) }()
	t.Cleanup(func() { cancel(); <-runDone })

	select {
	case <-fwd.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("forwarder did not become ready")
	}

	client, err := net.Dial("udp4", fwd.LocalAddr().String())
	if err != nil {
		t.Fatalf("dial forwarder = %v", err)
	}
	defer client.Close()

	query := []byte{0xAB, 0xCD, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for i := 0; i < 50; i++ {
		if _, err := client.Write(query); err != nil {
			t.Fatalf("write query %d = %v", i, err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fwd.SourceDropped() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("SourceDropped() = %d, want > 0 under single-source flood", fwd.SourceDropped())
}

func TestUpstreamResolversFallback(t *testing.T) {
	// A resolv.conf with only a loopback nameserver yields the public fallback.
	dir := t.TempDir()
	p := dir + "/resolv.conf"
	if err := os.WriteFile(p, []byte("nameserver 127.0.0.53\n"), 0o600); err != nil {
		t.Fatalf("write resolv.conf = %v", err)
	}
	got := upstreamResolversFrom([]string{p})
	if len(got) != 1 || got[0] != "1.1.1.1:53" {
		t.Errorf("upstreamResolversFrom = %v, want [1.1.1.1:53]", got)
	}
}

func TestUpstreamResolversParsesNonLoopback(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/resolv.conf"
	if err := os.WriteFile(p, []byte("nameserver 127.0.0.1\nnameserver 8.8.8.8\n"), 0o600); err != nil {
		t.Fatalf("write resolv.conf = %v", err)
	}
	got := upstreamResolversFrom([]string{p})
	if len(got) != 1 || got[0] != "8.8.8.8:53" {
		t.Errorf("upstreamResolversFrom = %v, want [8.8.8.8:53]", got)
	}
}
