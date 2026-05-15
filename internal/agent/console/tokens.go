// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/otherix/otherix/internal/auth"
)

// TokenTTL bounds the window between token issuance and the WebSocket
// upgrade that consumes it. 30 seconds is long enough for а
// human-driven `vms.console` -> `wss://` dial round-trip, short
// enough that an intercepted token cannot be hoarded into а future
// session.
const TokenTTL = 30 * time.Second

// gcInterval is the cadence the background sweep runs at. Picked к
// keep wall-clock skew from materially extending effective TTL while
// staying out of the CPU's way (most operator clusters open zero
// consoles per minute).
const gcInterval = 5 * time.Second

// Protocol enumerates the wire formats the agent's console-stream
// endpoint can speak after the WebSocket upgrade. Captured at
// token-issue time so the stream handler can dispatch к the right
// pump without re-parsing the request body.
type Protocol string

// Protocol enum values. Captured at token-issue time so the stream
// handler can dispatch к the right post-upgrade pump.
const (
	ProtocolVNC    Protocol = "vnc"
	ProtocolSerial Protocol = "serial"
)

// Valid reports whether p is one of the supported wire formats. Unset
// (zero value) is rejected so callers cannot fall through к а silent
// default.
func (p Protocol) Valid() bool {
	switch p {
	case ProtocolVNC, ProtocolSerial:
		return true
	}
	return false
}

// Token is the state Consume returns to the stream handler. The raw
// token plaintext is never carried in this struct — it exists only
// briefly on the issuance path; the store key is `sha256(plaintext)`
// (via auth.HashToken).
type Token struct {
	VMName    string
	Protocol  Protocol
	ExpiresAt time.Time
}

// Sentinel errors. The stream handler maps each к the corresponding
// HTTP status: ErrTokenNotFound / ErrTokenExpired / ErrTokenConsumed
// all surface as 401, ErrTokenVMMismatch surfaces as 401 too (the
// caller must not learn that the token exists for а different VM).
var (
	ErrTokenNotFound   = errors.New("console: token not found")
	ErrTokenExpired    = errors.New("console: token expired")
	ErrTokenConsumed   = errors.New("console: token already consumed")
	ErrTokenVMMismatch = errors.New("console: token bound to а different vm")
)

// TokenStore is the agent's in-memory short-lived token state.
// Concurrent-safe; methods take an internal mutex so callers don't
// have к synchronise externally. The store owns а background GC
// goroutine wired up by Start.
type TokenStore struct {
	mu     sync.Mutex
	tokens map[string]*tokenEntry // key = hex(sha256(plaintext))
	now    func() time.Time       // injected for tests
	ttl    time.Duration          // injected for tests; defaults к TokenTTL
}

type tokenEntry struct {
	vmName    string
	protocol  Protocol
	expiresAt time.Time
	consumed  bool
}

// NewTokenStore returns an empty store. Call Start before Issue/Consume
// to begin background sweeps; tests that drive the clock can skip
// Start and call SweepExpired directly.
func NewTokenStore() *TokenStore {
	return &TokenStore{
		tokens: make(map[string]*tokenEntry),
		now:    time.Now,
		ttl:    TokenTTL,
	}
}

// Start runs the background GC sweep until ctx is cancelled. Safe к
// call once per store; calling Start twice on the same store would
// double the sweep rate but is harmless. Tests that drive their own
// clock skip Start entirely.
func (s *TokenStore) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(gcInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.SweepExpired()
			}
		}
	}()
}

// Issue mints а fresh token bound к vmName + protocol, stores its
// sha256 digest, and returns the raw plaintext к the caller. The
// plaintext is the only copy of that value — the caller must hand it
// to the CP (and the CP к the WebSocket client) before it is lost.
// Issue rejects unsupported protocols up-front to keep invalid state
// off the store entirely.
func (s *TokenStore) Issue(vmName string, protocol Protocol) (raw string, expiresAt time.Time, err error) {
	if vmName == "" {
		return "", time.Time{}, errors.New("console: vm name is required")
	}
	if !protocol.Valid() {
		return "", time.Time{}, fmt.Errorf("console: unsupported protocol %q", protocol)
	}

	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", time.Time{}, fmt.Errorf("console: generate token: %v", err)
	}
	raw = hex.EncodeToString(buf[:])
	key := hex.EncodeToString(auth.HashToken(raw))

	s.mu.Lock()
	defer s.mu.Unlock()

	expiresAt = s.now().Add(s.ttl)
	s.tokens[key] = &tokenEntry{
		vmName:    vmName,
		protocol:  protocol,
		expiresAt: expiresAt,
	}
	return raw, expiresAt, nil
}

// Consume verifies the raw token, marks it consumed, and returns the
// state captured at issuance. The vmName argument cross-checks the
// path parameter against the token's binding — а token issued for vm
// А presented at vm B's stream endpoint returns ErrTokenVMMismatch.
// Concurrent consumers race for the single use; the loser sees
// ErrTokenConsumed.
func (s *TokenStore) Consume(raw, vmName string) (Token, error) {
	if raw == "" {
		return Token{}, ErrTokenNotFound
	}
	key := hex.EncodeToString(auth.HashToken(raw))

	s.mu.Lock()
	defer s.mu.Unlock()

	entry, ok := s.tokens[key]
	if !ok {
		return Token{}, ErrTokenNotFound
	}
	if entry.consumed {
		return Token{}, ErrTokenConsumed
	}
	if !s.now().Before(entry.expiresAt) {
		delete(s.tokens, key)
		return Token{}, ErrTokenExpired
	}
	if entry.vmName != vmName {
		return Token{}, ErrTokenVMMismatch
	}
	entry.consumed = true
	return Token{
		VMName:    entry.vmName,
		Protocol:  entry.protocol,
		ExpiresAt: entry.expiresAt,
	}, nil
}

// SweepExpired removes expired entries (including consumed ones —
// once consumed an entry has no future purpose). Exposed for tests
// driving а fake clock; production callers receive sweeps via Start.
func (s *TokenStore) SweepExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for key, entry := range s.tokens {
		if entry.consumed || !now.Before(entry.expiresAt) {
			delete(s.tokens, key)
		}
	}
}

// Size returns the number of live entries. Test-only; exported so the
// package's tests can assert sweep behaviour without poking at the
// internal map directly.
func (s *TokenStore) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokens)
}
