// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/otherix/otherix/internal/api/response"
)

// Timeout sets a context deadline of d on the request. If the deadline
// fires before the handler returns, Timeout writes a 503 with the standard
// error envelope.
//
// Known limitations tracked in ROADMAP "Open Questions":
//   - Late-write race: a slow handler may still write to w after the
//     deadline; net/http silently drops late writes/headers, so the wire
//     effect is at worst a duplicate log line. The production-grade fix
//     is a buffered ResponseWriter (cf. http.TimeoutHandler).
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
//   - A late panic (timeout already fired) hits the same "headers
//     already written" path documented for the Recoverer; the WriteError
//     call below is silently dropped by net/http, and the log line
//     remains the source of truth.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()

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
					response.WriteError(w, r,
						http.StatusInternalServerError,
						response.CodeInternal,
						"internal server error",
						nil,
					)
				}()
				next.ServeHTTP(w, r.WithContext(ctx))
			}()

			select {
			case <-done:
				return
			case <-ctx.Done():
				if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
					// Parent context cancelled (client gone) — nothing useful
					// to write to a writer the client is no longer reading.
					return
				}
				response.WriteError(w, r,
					http.StatusServiceUnavailable,
					response.CodeRequestTimeout,
					"request timeout",
					nil,
				)
			}
		})
	}
}
