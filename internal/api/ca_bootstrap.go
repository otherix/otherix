// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// ClusterCABootstrapStore is the storage surface the cluster-CA bootstrap
// depends on. *etcdstore.Store satisfies it.
type ClusterCABootstrapStore interface {
	ActiveCACert(ctx context.Context) (store.CaCert, error)
	CreateCACert(ctx context.Context, arg store.CreateCACertParams) (store.CaCert, error)
}

// BootstrapClusterCA syncs the on-disk cluster CA into the ca_certs
// store. Under the HA peer-PKI model the cluster CA is provisioned on
// disk before etcd starts (the peer-mTLS plane needs it pre-start); this
// hook then mirrors that same CA into etcd so the /v1/ca endpoint, the
// Step 2 CSR signer, and the CP leaf loader have an active row.
//
// It is idempotent and divergence-safe:
//   - etcd empty: insert the on-disk CA as the active row.
//   - etcd holds the same CA (fingerprint match): skip - already synced.
//   - etcd holds a different active CA: error. Disk and etcd disagreeing
//     on the trust root is an unexpected state (a stale etcd data dir, or
//     a cold multi-node bootstrap that did not share one CA); failing
//     fast beats serving two trust roots.
//
// Concurrent boots resolve via the uq_ca_certs_active index: the loser
// hits ErrCACertActiveExists, refetches, and accepts the winner's row
// only when it carries the same fingerprint (the joiners fetched the same
// shared CA), else surfaces the divergence.
//
// Call after the on-disk CA has been provisioned and before the HTTP
// server starts.
func BootstrapClusterCA(ctx context.Context, s ClusterCABootstrapStore, ca auth.ClusterCAResult, log *slog.Logger) error {
	existing, err := s.ActiveCACert(ctx)
	if err == nil {
		if !bytes.Equal(existing.FingerprintSha256, ca.Fingerprint) {
			return fmt.Errorf(
				"cluster CA divergence: on-disk CA %s does not match active etcd CA %s (reset the etcd data dir or distribute one shared CA)",
				hex.EncodeToString(ca.Fingerprint), hex.EncodeToString(existing.FingerprintSha256))
		}
		log.InfoContext(ctx, "cluster CA already synced to etcd, skipping",
			slog.String("ca_id", existing.ID.String()),
			slog.String("fingerprint_sha256", hex.EncodeToString(existing.FingerprintSha256)),
			slog.Time("not_after", existing.NotAfter))
		return nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("lookup active cluster CA: %v", err)
	}

	row, err := s.CreateCACert(ctx, store.CreateCACertParams{
		ID:                uuid.New(),
		CertPem:           ca.CertPEM,
		KeyPem:            ca.KeyPEM,
		FingerprintSha256: ca.Fingerprint,
		NotBefore:         ca.NotBefore,
		NotAfter:          ca.NotAfter,
	})
	if err != nil {
		// Concurrent boot lost the race — a sibling api-server inserted
		// first. Accept the winner only if it carries the same CA. Any
		// non-conflict error is fatal.
		if errors.Is(err, store.ErrCACertActiveExists) {
			winner, fetchErr := s.ActiveCACert(ctx)
			if fetchErr != nil {
				return fmt.Errorf("lost CA bootstrap race; refetch failed: %v", fetchErr)
			}
			if !bytes.Equal(winner.FingerprintSha256, ca.Fingerprint) {
				return fmt.Errorf(
					"cluster CA divergence after lost race: on-disk CA %s does not match sibling's CA %s",
					hex.EncodeToString(ca.Fingerprint), hex.EncodeToString(winner.FingerprintSha256))
			}
			log.WarnContext(ctx, "lost cluster CA bootstrap race, using sibling's matching row",
				slog.String("ca_id", winner.ID.String()),
				slog.String("fingerprint_sha256", hex.EncodeToString(winner.FingerprintSha256)))
			return nil
		}
		return fmt.Errorf("create cluster CA row: %v", err)
	}

	log.InfoContext(ctx, "synced cluster CA to etcd",
		slog.String("ca_id", row.ID.String()),
		slog.String("fingerprint_sha256", hex.EncodeToString(row.FingerprintSha256)),
		slog.Time("not_before", row.NotBefore),
		slog.Time("not_after", row.NotAfter),
		slog.String("algorithm", "ECDSA-P384"))
	return nil
}
