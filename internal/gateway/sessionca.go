// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package gateway

import (
	"context"
	"crypto"
	"log/slog"
	"sync/atomic"

	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/auth"
)

// sessionCAStore holds the public half of the cluster ingress-session CA the
// control plane distributes down-channel on every heartbeat. The connect gate
// reads it to verify short-lived ingress session credentials offline. It
// implements heartbeat.ResponseHandler so the same heartbeat sender that drives
// the reconcilers keeps the CA public half fresh.
//
// Updates are fail-open: a heartbeat with a missing or unparseable
// session_ca_public_pem leaves the last good value in place rather than
// clearing it, so a transient bad field never disarms verification. The connect
// gate fails closed on the bootstrap case (no CA ever received) by refusing the
// connect when current returns nil.
type sessionCAStore struct {
	log *slog.Logger
	pub atomic.Pointer[sessionCAKey]
}

// sessionCAKey pairs the parsed CA public key with the PEM it was parsed from,
// so a repeated identical heartbeat skips re-parsing.
type sessionCAKey struct {
	key crypto.PublicKey
	pem string
}

// newSessionCAStore builds an empty store. current returns nil until the first
// heartbeat carrying a parseable session CA public half arrives.
func newSessionCAStore(log *slog.Logger) *sessionCAStore {
	return &sessionCAStore{log: log}
}

// HandleHeartbeatResponse implements heartbeat.ResponseHandler. It parses and
// caches resp.SessionCAPublicPEM, keeping the last good value on any nil or
// unparseable field.
func (s *sessionCAStore) HandleHeartbeatResponse(_ context.Context, resp *heartbeat.Response) {
	if resp == nil || resp.SessionCAPublicPEM == nil {
		return
	}
	pem := *resp.SessionCAPublicPEM
	if cur := s.pub.Load(); cur != nil && cur.pem == pem {
		return
	}
	key, err := auth.ParseSessionCAPublic([]byte(pem))
	if err != nil {
		s.log.Warn("ignoring unparseable session ca public half from heartbeat", "error", err.Error())
		return
	}
	s.pub.Store(&sessionCAKey{key: key, pem: pem})
}

// current returns the latest cached session CA public half, or nil when none has
// been received yet.
func (s *sessionCAStore) current() crypto.PublicKey {
	if v := s.pub.Load(); v != nil {
		return v.key
	}
	return nil
}
