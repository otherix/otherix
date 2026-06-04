// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/otherix/otherix/internal/api/response"
)

// Timeout sets a context deadline of d on the request. If the deadline
// fires before the handler returns, Timeout writes a 503 with the standard
// error envelope.
//
// The downstream chain runs in a child goroutine and writes through a
// guardedWriter, mirroring net/http.TimeoutHandler. Access to the
// underlying ResponseWriter is mutually exclusive between the timeout-503
// path and any late handler write: once the deadline fires the parent
// stamps timedOut, and every subsequent write from the detached
// handler (including the buffered idempotency flush) becomes a no-op. That
// closes the data race where the parent's 503 and a late flush would both
// touch the same writer with no synchronisation.
//
// Known limitation tracked in ROADMAP "Open Questions":
//   - Goroutine outlives the timed-out request until the handler honours
//     ctx.Done() and returns; same caveat as http.TimeoutHandler.
//
// Panics from the spawned goroutine cannot be caught by the upstream
// Recoverer (different goroutine), so a defer-recover here writes the
// standard 500 envelope and logs the panic at ERROR — parallel to the
// Recoverer's behaviour. Two divergences from Recoverer, both forced by
// the goroutine boundary:
//
//   - http.ErrAbortHandler is swallowed silently here (no log, no
//     response). Re-panicking from a goroutine crashes the entire
//     process, so the Recoverer's re-panic contract cannot be honoured
//     verbatim. The outer select sees done close and returns with whatever
//     zero-value the ResponseWriter carries — which matches the spirit
//     of ErrAbortHandler ("abort silently").
//   - A late panic (timeout already fired) writes through the guarded
//     writer; once timedOut is set the WriteError call below is a no-op,
//     and the log line remains the source of truth.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()

			gw := &guardedWriter{w: w}

			done := make(chan struct{})
			go func() {
				defer close(done)
				defer func() {
					rec := recover()
					if rec == nil {
						return
					}
					if rec == http.ErrAbortHandler {
						// ErrAbortHandler asks for a silent abort. The
						// goroutine boundary prevents us from re-panicking
						// (would crash the process), so swallow without a
						// log line or a response — same wire effect as
						// the sentinel's net/http-server-side intent.
						return
					}
					slog.Default().ErrorContext(r.Context(),
						"panic in handler (timeout middleware goroutine)",
						slog.Any("panic", rec),
						slog.String("stack", string(debug.Stack())),
						slog.String("request_id", RequestIDFromContext(r.Context())),
					)
					response.WriteError(gw, r,
						http.StatusInternalServerError,
						response.CodeInternal,
						"internal server error",
						nil,
					)
				}()
				next.ServeHTTP(gw, r.WithContext(ctx))
			}()

			select {
			case <-done:
				return
			case <-ctx.Done():
				if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
					// Parent context cancelled (client gone) — nothing useful
					// to write to a writer the client is no longer reading.
					// Still latch timedOut so a late handler write does not
					// race the abandoned writer.
					gw.timeout(nil)
					return
				}
				gw.timeout(func(raw http.ResponseWriter) {
					response.WriteError(raw, r,
						http.StatusServiceUnavailable,
						response.CodeRequestTimeout,
						"request timeout",
						nil,
					)
				})
			}
		})
	}
}

// guardedWriter serialises access to an underlying ResponseWriter shared
// between the Timeout parent goroutine and the detached handler goroutine.
// It mirrors the writer net/http.TimeoutHandler installs: once the
// deadline fires the parent stamps timedOut, after which every handler
// write is dropped so the client keeps the 503 the parent already sent.
type guardedWriter struct {
	mu          sync.Mutex
	w           http.ResponseWriter
	timedOut    bool
	wroteHeader bool
}

// Header returns the underlying header map. It is read/written only before
// the first WriteHeader, so it needs no further guarding beyond the mutex
// the write path already holds.
func (g *guardedWriter) Header() http.Header {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.w.Header()
}

// WriteHeader forwards the status to the underlying writer unless the
// request has already timed out or a header was already written.
func (g *guardedWriter) WriteHeader(status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timedOut || g.wroteHeader {
		return
	}
	g.wroteHeader = true
	g.w.WriteHeader(status)
}

// Write forwards the body to the underlying writer unless the request has
// already timed out. A nil-or-implicit header is materialised first, as
// net/http does.
func (g *guardedWriter) Write(b []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timedOut {
		// The client already received the 503; drop the late write so it
		// cannot corrupt the response or race the abandoned writer.
		return len(b), nil
	}
	g.wroteHeader = true
	return g.w.Write(b)
}

// timeout latches the timed-out state, after which every handler write is
// dropped. If write503 is non-nil and no handler write had landed yet, it
// is invoked under the lock against the raw underlying writer to emit the
// 503 envelope. Running it under the lock and before timedOut takes effect
// keeps the parent's 503 from being swallowed by the drop logic while
// still excluding any concurrent handler write. When the handler already
// wrote a full response before the deadline fired, write503 is skipped so
// the 503 is not appended.
func (g *guardedWriter) timeout(write503 func(http.ResponseWriter)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.timedOut {
		return
	}
	if write503 != nil && !g.wroteHeader {
		g.wroteHeader = true
		write503(g.w)
	}
	g.timedOut = true
}
