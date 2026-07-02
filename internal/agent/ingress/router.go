// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ingress

import (
	"github.com/go-chi/chi/v5"
)

// MountConnectRoutes mounts the credential-gated POST /v1/connect route on r.
// The route verifies a short-lived ingress session credential (its credential
// gate, not CP identity) and then hijacks the inbound connection and splices it
// to the credential's guest target on the overlay. It is mounted in its own
// group so the caller can keep it outside a per-request timeout: a long-lived
// spliced session must not be killed by the per-request deadline, and the
// timeout's guarded writer does not support hijacking.
func MountConnectRoutes(r chi.Router, h *ConnectHandler) {
	r.Group(func(r chi.Router) {
		r.Use(h.VerifyCred)
		r.Post("/v1/connect", h.Connect)
	})
}
