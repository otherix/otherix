// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/otherix/otherix/internal/api/response"
)

// Recoverer catches panics from downstream handlers, logs the panic value
// and the stack at error level, and writes a 500 in the standard error
// envelope.
//
// If the panicked handler had already started writing the response body
// before panicking, the headers cannot be changed and WriteError's
// WriteHeader call is silently ignored by net/http; this matches the
// behaviour of chi.Recoverer and is acceptable for an MVP. The log line
// is the source of truth for diagnosing such cases.
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler {
					// net/http documents this as a sentinel for handlers that
					// want to abort without logging. Re-panic to preserve that
					// contract.
					panic(rec)
				}
				log.ErrorContext(r.Context(),
					"panic in handler",
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

			next.ServeHTTP(w, r)
		})
	}
}
