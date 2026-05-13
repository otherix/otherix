// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// BootstrapClusterCA ensures the ca_certs table holds an active
// cluster CA row. On а fresh DB it generates an ECDSA P-384 self-
// signed CA и persists it; on subsequent boots it
// observes the existing row и returns. Concurrent boots resolve via
// the uq_ca_certs_active partial unique index — the loser hits 23505,
// falls through к GetActiveCACert, и proceeds with the winner's row.
//
// Call after migrations have been applied и before the HTTP server
// starts. Required by the /v1/ca endpoint и by the Step 2 CSR
// signing handler.
func BootstrapClusterCA(ctx context.Context, s *store.Store, log *slog.Logger) error {
	return bootstrapClusterCAWithClock(ctx, s, log, time.Now)
}

// bootstrapClusterCAWithClock is the clock-injectable form of
// BootstrapClusterCA. Tests pass а fixed clock к assert deterministic
// not_before / not_after fields; production wiring uses time.Now.
func bootstrapClusterCAWithClock(ctx context.Context, s *store.Store, log *slog.Logger, now func() time.Time) error {
	existing, err := s.Queries().GetActiveCACert(ctx)
	if err == nil {
		log.InfoContext(ctx, "cluster CA already provisioned, skipping generation",
			slog.String("ca_id", existing.ID.String()),
			slog.String("fingerprint_sha256", hex.EncodeToString(existing.FingerprintSha256)),
			slog.Time("not_after", existing.NotAfter))
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lookup active cluster CA: %v", err)
	}

	result, err := auth.GenerateClusterCA(now())
	if err != nil {
		return fmt.Errorf("generate cluster CA: %v", err)
	}

	row, err := s.Queries().CreateCACert(ctx, store.CreateCACertParams{
		ID:                uuid.New(),
		CertPem:           result.CertPEM,
		KeyPem:            result.KeyPEM,
		FingerprintSha256: result.Fingerprint,
		NotBefore:         result.NotBefore,
		NotAfter:          result.NotAfter,
	})
	if err != nil {
		// Concurrent boot lost the race — а sibling api-server inserted
		// first. Fetch the winner и return success. Any non-unique-
		// violation error is fatal.
		if isUniqueViolation(err) {
			winner, fetchErr := s.Queries().GetActiveCACert(ctx)
			if fetchErr != nil {
				return fmt.Errorf("lost CA bootstrap race; refetch failed: %v", fetchErr)
			}
			log.WarnContext(ctx, "lost cluster CA bootstrap race, using sibling's row",
				slog.String("ca_id", winner.ID.String()),
				slog.String("fingerprint_sha256", hex.EncodeToString(winner.FingerprintSha256)))
			return nil
		}
		return fmt.Errorf("create cluster CA row: %v", err)
	}

	log.InfoContext(ctx, "generated cluster CA",
		slog.String("ca_id", row.ID.String()),
		slog.String("fingerprint_sha256", hex.EncodeToString(row.FingerprintSha256)),
		slog.Time("not_before", row.NotBefore),
		slog.Time("not_after", row.NotAfter),
		slog.String("algorithm", "ECDSA-P384"),
		slog.Duration("validity", auth.ClusterCAValidity))
	return nil
}
