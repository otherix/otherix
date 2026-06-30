// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// SessionCABootstrapStore is the storage surface the ingress-session CA
// bootstrap depends on. *etcdstore.Store satisfies it.
type SessionCABootstrapStore interface {
	ActiveSessionCA(ctx context.Context) (store.SessionCA, error)
	CreateSessionCA(ctx context.Context, arg store.CreateSessionCAParams) (store.SessionCA, error)
}

// BootstrapSessionCA provisions the cluster ingress-session certificate
// authority in etcd on first boot so every HA replica signs session credentials
// with the same CA and gateways verify them against a single public half. It is
// idempotent and race-safe: an existing active row is a no-op, otherwise it
// generates a fresh ECDSA P-384 session CA and creates the active row.
// Concurrent boots converge via the at-most-one-active guard in CreateSessionCA,
// which returns the winner's row to the loser. The CA is a distinct key from the
// cluster CA, so a leaked session credential never widens mesh trust; the private
// key never leaves the control plane and is never logged.
//
// Call after BootstrapSSHUserCA and before the HTTP server starts.
func BootstrapSessionCA(ctx context.Context, s SessionCABootstrapStore, log *slog.Logger) error {
	existing, err := s.ActiveSessionCA(ctx)
	if err == nil {
		log.InfoContext(ctx, "cluster ingress-session CA already provisioned, skipping",
			slog.String("session_ca_id", existing.ID.String()),
			slog.Time("created_at", existing.CreatedAt))
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("lookup active session CA: %v", err)
	}

	mat, err := auth.GenerateSessionCA()
	if err != nil {
		return fmt.Errorf("generate session CA: %v", err)
	}
	row, err := s.CreateSessionCA(ctx, store.CreateSessionCAParams{
		PrivateKeyPEM: mat.PrivateKeyPEM,
		PublicKeyPEM:  mat.PublicKeyPEM,
	})
	if err != nil {
		return fmt.Errorf("create session CA row: %v", err)
	}

	log.InfoContext(ctx, "provisioned cluster ingress-session CA",
		slog.String("session_ca_id", row.ID.String()),
		slog.Time("created_at", row.CreatedAt),
		slog.String("algorithm", "ECDSA-P384"))
	return nil
}
