// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package wskeepalive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

// TestRun_CancelsOnUnresponsivePeer pins the core contract: a peer that
// stops responding (half-open connection - it never reads, so it never
// auto-pongs our pings) is detected and the session is cancelled. This is
// the leak fix: on the agent this cancel unwinds the console pump and
// frees the single-console slot instead of stalling forever.
func TestRun_CancelsOnUnresponsivePeer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		// Never Read -> never auto-pong: a hung / half-open peer.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(srv.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// A concurrent reader so Ping can observe pongs (Ping does not read the
	// connection itself). There are none here, so every Ping times out.
	go func() { _, _, _ = conn.Read(ctx) }()

	done := make(chan struct{})
	go func() {
		Run(ctx, cancel, conn, 20*time.Millisecond, 60*time.Millisecond)
		close(done)
	}()

	select {
	case <-ctx.Done(): // keepalive detected the dead peer and cancelled.
	case <-time.After(2 * time.Second):
		t.Fatal("keepalive did not cancel the session for an unresponsive peer")
	}
	<-done
}

// TestRun_SurvivesResponsivePeer pins that a live, responsive peer is NOT
// cancelled: the server keeps reading (so it auto-pongs), and the session
// survives several ping cycles. Guards against a keepalive that false-trips
// on a healthy interactive console.
func TestRun_SurvivesResponsivePeer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		// Keep reading so incoming pings are auto-ponged.
		for {
			if _, _, rerr := c.Read(r.Context()); rerr != nil {
				return
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(srv.URL), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	go func() { _, _, _ = conn.Read(ctx) }()

	go Run(ctx, cancel, conn, 20*time.Millisecond, 200*time.Millisecond)

	select {
	case <-ctx.Done():
		t.Fatal("keepalive cancelled a responsive peer")
	case <-time.After(300 * time.Millisecond):
		// Survived multiple ping cycles against a responsive peer.
	}
}
