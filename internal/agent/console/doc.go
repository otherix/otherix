// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package console hosts the agent-side in-memory primitives the VM
// console flow depends on: a short-lived single-use TokenStore and a
// per-VM single-session ConnectionTracker. Both are intentionally
// process-local. Agent
// restarts kill the QEMU `-serial unix:` socket alongside the agent
// itself, so persisting console state across restarts has no value —
// the socket the persisted state would point at is gone too.
//
// Token storage form mirrors the rest of the platform: agent stores
// only `sha256(plaintext)` (via auth.HashToken); the plaintext is
// returned to the CP once at issuance and never persisted. Single-use
// semantics + 30-second TTL bound exposure window; the GC goroutine
// sweeps expired entries on a fixed cadence so the store doesn't
// leak memory under load.
package console
