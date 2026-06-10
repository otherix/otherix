// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package wskeepalive provides application-level liveness detection for a
// long-lived WebSocket: it periodically pings the peer and cancels the
// session when a pong does not arrive in time.
//
// The console path needs this because both the agent console pump and the
// CP relay clear their hijacked read/write deadlines (an interactive
// operator may sit idle for minutes), so a half-open connection - a laptop
// that slept, a dropped network, a CLI killed without a clean close frame -
// would otherwise leave `wsConn.Read` blocked forever. On the agent that
// stalls the deferred subscriber Close and leaks the single-console slot
// (`ErrConsoleInUse`) until the agent restarts. A ping with a bounded
// wait turns a dead peer into a prompt session cancel, which unwinds the
// pumps and frees the slot. TCP keepalives default to ~2h and are far too
// slow for this.
package wskeepalive

import (
	"context"
	"time"

	"github.com/coder/websocket"
)

// DefaultInterval and DefaultTimeout pace the console keepalive: a ping
// every DefaultInterval, waiting up to DefaultTimeout for the pong, so an
// unresponsive peer is detected within ~DefaultInterval+DefaultTimeout.
// Chosen generous enough that a brief network blip on a live interactive
// session does not trip a false disconnect.
const (
	DefaultInterval = 15 * time.Second
	DefaultTimeout  = 10 * time.Second
)

// Run pings conn every interval, waiting up to timeout for each pong, and
// calls cancel the first time a ping fails - the peer is gone or the
// connection is half-open. It returns when ctx is done (a sibling pump
// already unwound) or after it has cancelled. conn.Ping requires a
// concurrent Reader to read the pong; every console pump that uses Run
// already runs one.
func Run(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, interval, timeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, timeout)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				cancel()
				return
			}
		}
	}
}
