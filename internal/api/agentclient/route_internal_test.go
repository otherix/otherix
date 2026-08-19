// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// connectRoute is a minimal route for the connectNode-level tests.
var connectRoute = AgentRoute{
	GatewaySAN:    "node-gw.agents.otherix.local",
	TargetOverlay: "10.0.0.5:9443",
}

// TestConnectNode_StallHonorsDeadline proves the time bound: a gateway that reads
// the CONNECT request then never answers must not wedge the dial goroutine. With
// a ctx deadline set, connectNode returns a bounded deadline error; without the
// SetDeadline fix the raw read would block forever (the 5s guard would fire).
func TestConnectNode_StallHonorsDeadline(t *testing.T) {
	t.Parallel()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	go func() { _, _ = io.Copy(io.Discard, server) }() // read the request, never answer

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- connectNode(ctx, client, connectRoute) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("connectNode(stalling gateway) = nil, want a bounded deadline error")
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("connectNode err = %v, want a deadline-exceeded error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("connectNode did not honor the connect deadline - it hung on the stalling gateway")
	}
}

// TestConnectNode_RejectsOversizedHeaders proves the byte bound: a gateway that
// streams headers past maxConnectHeaderBytes without a terminator is rejected
// promptly (via the LimitReader EOF), not read unboundedly into memory or left to
// hang until the time deadline.
func TestConnectNode_RejectsOversizedHeaders(t *testing.T) {
	t.Parallel()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	go func() { _, _ = io.Copy(io.Discard, server) }() // consume the CONNECT request
	go func() {
		_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\n")
		// A header stream well past the cap, with no blank-line terminator.
		flood := bytes.Repeat([]byte("X-Flood: aaaaaaaaaaaaaaaa\r\n"), 8000)
		_, _ = server.Write(flood)
	}()

	done := make(chan error, 1)
	go func() { done <- connectNode(context.Background(), client, connectRoute) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("connectNode(oversized headers) = nil, want a byte-bound rejection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("connectNode did not reject oversized headers within 5s (byte bound missing?)")
	}
}

// TestConnectNode_ClearsDeadlineOnSuccess proves the load-bearing clear: after a
// 200 the connect deadline must be removed so the inner handshake / later I/O is
// not bounded by it. The test reads a byte the gateway sends AFTER the connect
// deadline would have fired; a stale deadline would fail that read.
func TestConnectNode_ClearsDeadlineOnSuccess(t *testing.T) {
	t.Parallel()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	go func() { _, _ = io.Copy(io.Discard, server) }() // consume the CONNECT request
	go func() {
		_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\n\r\n")
		time.Sleep(600 * time.Millisecond) // later than the 250ms connect deadline
		_, _ = server.Write([]byte("X"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := connectNode(ctx, client, connectRoute); err != nil {
		t.Fatalf("connectNode: %v", err)
	}

	// The connect deadline (250ms) is in the past by now; this read blocks ~600ms
	// and must succeed - proving the deadline was cleared, not inherited.
	buf := make([]byte, 1)
	if _, err := client.Read(buf); err != nil {
		t.Fatalf("post-CONNECT read failed - stale connect deadline not cleared: %v", err)
	}
	if buf[0] != 'X' {
		t.Errorf("post-CONNECT read = %q, want X", buf[0])
	}
}
