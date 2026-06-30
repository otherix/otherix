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

// SSHUserCABootstrapStore is the storage surface the SSH user-CA bootstrap
// depends on. *etcdstore.Store satisfies it.
type SSHUserCABootstrapStore interface {
	ActiveSSHUserCA(ctx context.Context) (store.SSHUserCA, error)
	CreateSSHUserCA(ctx context.Context, arg store.CreateSSHUserCAParams) (store.SSHUserCA, error)
}

// BootstrapSSHUserCA provisions the cluster SSH user certificate authority in
// etcd on first boot so every HA replica loads the same CA and can sign guest
// user-certs. It is idempotent and race-safe: an existing active row is a no-op,
// otherwise it generates a fresh ECDSA P-384 SSH CA and creates the active row.
// Concurrent boots converge via the at-most-one-active guard in CreateSSHUserCA,
// which returns the winner's row to the loser. The CA private key never leaves
// the control plane and is never logged.
//
// Call after BootstrapClusterCA and before the HTTP server starts.
func BootstrapSSHUserCA(ctx context.Context, s SSHUserCABootstrapStore, log *slog.Logger) error {
	existing, err := s.ActiveSSHUserCA(ctx)
	if err == nil {
		log.InfoContext(ctx, "cluster SSH user CA already provisioned, skipping",
			slog.String("ssh_ca_id", existing.ID.String()),
			slog.Time("created_at", existing.CreatedAt))
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("lookup active SSH user CA: %v", err)
	}

	mat, err := auth.GenerateSSHUserCA()
	if err != nil {
		return fmt.Errorf("generate SSH user CA: %v", err)
	}
	row, err := s.CreateSSHUserCA(ctx, store.CreateSSHUserCAParams{
		PrivateKeyPEM:       mat.PrivateKeyPEM,
		PublicKeyAuthorized: mat.PublicKeyAuthorized,
	})
	if err != nil {
		return fmt.Errorf("create SSH user CA row: %v", err)
	}

	log.InfoContext(ctx, "provisioned cluster SSH user CA",
		slog.String("ssh_ca_id", row.ID.String()),
		slog.Time("created_at", row.CreatedAt),
		slog.String("algorithm", "ECDSA-P384"))
	return nil
}
