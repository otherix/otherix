// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package middleware

import "net/http"

// MaxBodyBytes caps the request body at limit bytes by wrapping r.Body
// in http.MaxBytesReader, bounding the memory an unauthenticated client
// can force the server to buffer (the anonymous bodied endpoints --
// auth.login, nodes.join, cluster.join -- are the motivating case). A
// read past the limit fails with *http.MaxBytesError; the existing
// JSON-decode error paths surface that as 400 validation_failed, which
// is acceptable here -- no bespoke 413 handling. When limit <= 0 the
// middleware is a pass-through (no wrapping) so callers can disable it.
func MaxBodyBytes(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if limit <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}
