// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

// internalNodeParams builds a minimal valid node for the internal cert tests.
// It mirrors the external etcdstore_test nodeParams, duplicated here because the
// internal test package cannot reach that helper.
func internalNodeParams(name string) store.CreateNodeParams {
	return store.CreateNodeParams{
		ID:                      uuid.New(),
		Name:                    name,
		Architecture:            store.CpuArchAmd64,
		AdvertisedEndpoint:      "https://node.example:9443",
		MigrationHost:           "10.0.0.1",
		MigrationPortRangeStart: 49152,
		MigrationPortRangeEnd:   49251,
		Status:                  store.NodeStatusPending,
	}
}

// TestDeleteNodeRevokesAgentCerts proves that deleting a node revokes its agent
// certs atomically with the soft-delete: the by-fingerprint index (the exact
// lookup the mTLS authn path makes) misses, the node index drops, and the
// primary row keeps a revoked_at audit stamp. Covers both the force=false path
// on an empty node and the force=true path.
func TestDeleteNodeRevokesAgentCerts(t *testing.T) {
	for _, force := range []bool{false, true} {
		force := force
		name := "revokes"
		if force {
			name += "-force"
		}
		t.Run(name, func(t *testing.T) {
			s := startInternalStore(t)
			ctx := context.Background()

			node, err := s.CreateNode(ctx, internalNodeParams(uniqueInternalNodeName("doomed")))
			if err != nil {
				t.Fatalf("CreateNode: %v", err)
			}
			fp := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
			cert, err := s.CreateAgentCert(ctx, store.CreateAgentCertParams{
				ID: uuid.New(), NodeID: node.ID, Serial: []byte{1},
				FingerprintSha256: fp, SubjectDn: "CN=node-doomed",
				NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			})
			if err != nil {
				t.Fatalf("CreateAgentCert: %v", err)
			}

			out, err := s.DeleteNode(ctx, node.ID, force, uuid.New())
			if err != nil {
				t.Fatalf("DeleteNode: %v", err)
			}
			if out.CertsRevoked != 1 {
				t.Errorf("CertsRevoked = %d, want 1", out.CertsRevoked)
			}

			// The mTLS lookup call now misses: the deleted node can no longer
			// authenticate.
			if _, err := s.AgentCertByFingerprint(ctx, fp); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("AgentCertByFingerprint after delete = %v, want ErrNotFound", err)
			}
			// No node-index leak.
			has, err := s.NodeHasActiveCert(ctx, node.ID)
			if err != nil {
				t.Fatalf("NodeHasActiveCert: %v", err)
			}
			if has {
				t.Error("NodeHasActiveCert = true after delete, want false")
			}
			// Audit trail preserved on the primary row.
			var stored store.AgentCert
			ok, err := s.c.GetJSON(ctx, agentCertKey(cert.ID), &stored)
			if err != nil || !ok {
				t.Fatalf("read primary cert: ok=%v err=%v", ok, err)
			}
			if stored.RevokedAt == nil {
				t.Error("primary RevokedAt = nil after delete, want set")
			}
		})
	}
}

// TestRevokeNodeAgentCertsOps_StaleIndex proves revocation is robust when a
// node-index entry outlives its primary row: the stale index entry is cleaned
// up (not stranded) and no primary is counted as revoked.
func TestRevokeNodeAgentCertsOps_StaleIndex(t *testing.T) {
	s := startInternalStore(t)
	ctx := context.Background()
	node, err := s.CreateNode(ctx, internalNodeParams(uniqueInternalNodeName("stale")))
	if err != nil {
		t.Fatalf("CreateNode: %v", err)
	}

	// Plant a node-index entry pointing at a cert id with no primary row.
	orphanID := uuid.New()
	if _, err := s.c.Raw().Put(ctx,
		agentCertNodeIndexKey(node.ID, orphanID), orphanID.String()); err != nil {
		t.Fatalf("plant stale index: %v", err)
	}

	ops, count, err := s.revokeNodeAgentCertsOps(ctx, node.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("revokeNodeAgentCertsOps: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 (no primary to revoke)", count)
	}
	// The stale index entry is cleaned up, not left behind.
	if _, err := s.c.Raw().Txn(ctx).Then(ops...).Commit(); err != nil {
		t.Fatalf("commit ops: %v", err)
	}
	has, err := s.NodeHasActiveCert(ctx, node.ID)
	if err != nil {
		t.Fatalf("NodeHasActiveCert: %v", err)
	}
	if has {
		t.Error("stale node-index entry survived revocation")
	}
}

func uniqueInternalNodeName(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}
