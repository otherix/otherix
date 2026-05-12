// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package middleware provides the HTTP middleware stack used by the
// api-server: request id propagation, structured slog request logging,
// panic recovery into the standard error envelope, and per-request
// timeout enforcement. Middleware is wired up by internal/api.NewRouter
// in a fixed order; see comments there for rationale.
package middleware

type contextKey string

const requestIDKey contextKey = "request_id"

// HeaderRequestID is the HTTP header used both to receive a request id
// from the caller and to echo the (possibly generated) id back in the
// response.
const HeaderRequestID = "X-Request-ID"
