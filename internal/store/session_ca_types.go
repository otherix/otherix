// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"time"

	"github.com/google/uuid"
)

// SessionCA is the cluster-wide ingress-session certificate authority persisted
// in etcd: the PEM-encoded private key the control plane signs short-lived
// ingress session credentials with, and the PEM-encoded public half distributed
// to gateways so they verify those credentials offline. It is distinct from the
// cluster CA, so a leaked session credential never widens mesh trust. There is
// one active row per cluster (the at-most-one-active guard), so every HA replica
// signs with the same key. The private key never leaves the control plane.
type SessionCA struct {
	ID            uuid.UUID
	PrivateKeyPEM []byte
	PublicKeyPEM  []byte
	CreatedAt     time.Time
}

// CreateSessionCAParams carries the freshly generated ingress-session-CA
// material the store persists. The store assigns the id and created_at.
type CreateSessionCAParams struct {
	PrivateKeyPEM []byte
	PublicKeyPEM  []byte
}
