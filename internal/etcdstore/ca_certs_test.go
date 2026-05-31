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

	authhandlers "github.com/otherix/otherix/internal/api/handlers/auth"
	cahandlers "github.com/otherix/otherix/internal/api/handlers/ca"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd-backed store satisfies the ca and auth handler contracts (the leaf
// seams that depend only on ActiveCACert and UserByEmail respectively).
var (
	_ cahandlers.Store   = (*etcdstore.Store)(nil)
	_ authhandlers.Store = (*etcdstore.Store)(nil)
)

func caParams() store.CreateCACertParams {
	now := time.Now().UTC()
	return store.CreateCACertParams{
		ID:                uuid.New(),
		CertPem:           []byte("cert"),
		KeyPem:            []byte("key"),
		FingerprintSha256: []byte("fingerprint"),
		NotBefore:         now,
		NotAfter:          now.Add(24 * time.Hour),
	}
}

func TestActiveCACertNotFoundWhenUnprovisioned(t *testing.T) {
	s, _ := startStore(t)
	if _, err := s.ActiveCACert(context.Background()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ActiveCACert(fresh) = %v, want store.ErrNotFound", err)
	}
}

func TestCreateAndGetActiveCACert(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := caParams()
	created, err := s.CreateCACert(ctx, p)
	if err != nil {
		t.Fatalf("CreateCACert: %v", err)
	}
	if !created.Active || created.CreatedAt.IsZero() {
		t.Errorf("CreateCACert = %+v, want active + created_at stamped", created)
	}
	got, err := s.ActiveCACert(ctx)
	if err != nil {
		t.Fatalf("ActiveCACert: %v", err)
	}
	if got.ID != p.ID || string(got.CertPem) != "cert" {
		t.Errorf("ActiveCACert = %+v, want id=%v cert=cert", got, p.ID)
	}
}

func TestCreateCACertSecondActiveConflicts(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	if _, err := s.CreateCACert(ctx, caParams()); err != nil {
		t.Fatalf("first CreateCACert: %v", err)
	}
	if _, err := s.CreateCACert(ctx, caParams()); !errors.Is(err, store.ErrCACertActiveExists) {
		t.Errorf("second CreateCACert = %v, want store.ErrCACertActiveExists", err)
	}
}

func TestListTrustedCACertsEmpty(t *testing.T) {
	s, _ := startStore(t)
	got, err := s.ListTrustedCACerts(context.Background())
	if err != nil {
		t.Fatalf("ListTrustedCACerts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListTrustedCACerts(fresh) = %d certs, want 0", len(got))
	}
}

func TestListTrustedCACertsReturnsActiveSigner(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	p := caParams()
	if _, err := s.CreateCACert(ctx, p); err != nil {
		t.Fatalf("CreateCACert: %v", err)
	}
	got, err := s.ListTrustedCACerts(ctx)
	if err != nil {
		t.Fatalf("ListTrustedCACerts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListTrustedCACerts = %d certs, want 1", len(got))
	}
	if got[0].ID != p.ID {
		t.Errorf("trusted CA id = %v, want %v", got[0].ID, p.ID)
	}
	if got[0].RetiredAt != nil {
		t.Errorf("trusted CA retired_at = %v, want nil", got[0].RetiredAt)
	}
}

func TestListTrustedCACertsExcludesExpired(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	expired := store.CreateCACertParams{
		ID:                uuid.New(),
		CertPem:           []byte("cert"),
		KeyPem:            []byte("key"),
		FingerprintSha256: []byte("fingerprint"),
		NotBefore:         now.Add(-48 * time.Hour),
		NotAfter:          now.Add(-24 * time.Hour),
	}
	if _, err := s.CreateCACert(ctx, expired); err != nil {
		t.Fatalf("CreateCACert(expired): %v", err)
	}
	got, err := s.ListTrustedCACerts(ctx)
	if err != nil {
		t.Fatalf("ListTrustedCACerts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ListTrustedCACerts(expired CA) = %d certs, want 0 (expired excluded)", len(got))
	}
}

func TestDeactivateCACertsAllowsReprovision(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()
	if _, err := s.CreateCACert(ctx, caParams()); err != nil {
		t.Fatalf("CreateCACert: %v", err)
	}
	if err := s.DeactivateCACerts(ctx); err != nil {
		t.Fatalf("DeactivateCACerts: %v", err)
	}
	if _, err := s.ActiveCACert(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ActiveCACert after deactivate = %v, want store.ErrNotFound", err)
	}
	// Deactivate is idempotent and a fresh active row may be created.
	if err := s.DeactivateCACerts(ctx); err != nil {
		t.Errorf("DeactivateCACerts(idempotent): %v", err)
	}
	if _, err := s.CreateCACert(ctx, caParams()); err != nil {
		t.Errorf("CreateCACert after deactivate: %v", err)
	}
}
