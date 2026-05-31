// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestAgentCertByFingerprint(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	np := nodeParams(uniqueNodeName("agentcert"))
	if _, err := s.CreateNode(ctx, np); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	fp := []byte("fingerprint-abc")
	if _, err := s.CreateAgentCert(ctx, store.CreateAgentCertParams{
		ID: uuid.New(), NodeID: np.ID, Serial: []byte("serial"), FingerprintSha256: fp,
		SubjectDn: "CN=node-x", NotBefore: time.Now().UTC(), NotAfter: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateAgentCert: %v", err)
	}

	got, err := s.AgentCertByFingerprint(ctx, fp)
	if err != nil || got.NodeID != np.ID || got.RevokedAt != nil {
		t.Errorf("AgentCertByFingerprint = (%+v, %v), want node %v active", got, err, np.ID)
	}
	if _, err := s.AgentCertByFingerprint(ctx, []byte("nope")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("AgentCertByFingerprint(missing) = %v, want ErrNotFound", err)
	}
}
