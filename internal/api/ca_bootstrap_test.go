// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package api_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// fakeCABootstrapStore is a minimal ClusterCABootstrapStore double. active holds
// the current row (nil = ErrNotFound). When createErr is set, CreateCACert
// returns it and adopts raceWinner as the active row, modelling a lost
// uq_ca_certs_active race.
type fakeCABootstrapStore struct {
	active      *store.CaCert
	createErr   error
	raceWinner  *store.CaCert
	createCalls int
}

func (f *fakeCABootstrapStore) ActiveCACert(_ context.Context) (store.CaCert, error) {
	if f.active == nil {
		return store.CaCert{}, store.ErrNotFound
	}
	return *f.active, nil
}

func (f *fakeCABootstrapStore) CreateCACert(_ context.Context, arg store.CreateCACertParams) (store.CaCert, error) {
	f.createCalls++
	if f.createErr != nil {
		if f.raceWinner != nil {
			f.active = f.raceWinner
		}
		return store.CaCert{}, f.createErr
	}
	row := store.CaCert{
		ID:                arg.ID,
		CertPem:           arg.CertPem,
		KeyPem:            arg.KeyPem,
		FingerprintSha256: arg.FingerprintSha256,
		NotBefore:         arg.NotBefore,
		NotAfter:          arg.NotAfter,
		Active:            true,
	}
	f.active = &row
	return row, nil
}

func testCA(t *testing.T) auth.ClusterCAResult {
	t.Helper()
	ca, err := auth.GenerateClusterCA(time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	return ca
}

func rowFromCA(ca auth.ClusterCAResult) *store.CaCert {
	return &store.CaCert{
		ID:                uuid.New(),
		CertPem:           ca.CertPEM,
		KeyPem:            ca.KeyPEM,
		FingerprintSha256: ca.Fingerprint,
		NotBefore:         ca.NotBefore,
		NotAfter:          ca.NotAfter,
		Active:            true,
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBootstrapClusterCAInsertsWhenEtcdEmpty(t *testing.T) {
	ca := testCA(t)
	s := &fakeCABootstrapStore{}

	if err := api.BootstrapClusterCA(context.Background(), s, ca, discardLogger()); err != nil {
		t.Fatalf("BootstrapClusterCA(empty store): %v", err)
	}
	if s.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", s.createCalls)
	}
	if s.active == nil {
		t.Fatal("active row not set after insert")
	}
	if string(s.active.FingerprintSha256) != string(ca.Fingerprint) {
		t.Errorf("inserted fingerprint = %x, want %x", s.active.FingerprintSha256, ca.Fingerprint)
	}
}

func TestBootstrapClusterCASkipsWhenAlreadySynced(t *testing.T) {
	ca := testCA(t)
	s := &fakeCABootstrapStore{active: rowFromCA(ca)}

	if err := api.BootstrapClusterCA(context.Background(), s, ca, discardLogger()); err != nil {
		t.Fatalf("BootstrapClusterCA(already synced): %v", err)
	}
	if s.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0 - the on-disk CA already matches etcd", s.createCalls)
	}
}

func TestBootstrapClusterCADivergenceErrors(t *testing.T) {
	disk := testCA(t)
	other := testCA(t) // an independently generated CA: different fingerprint
	s := &fakeCABootstrapStore{active: rowFromCA(other)}

	err := api.BootstrapClusterCA(context.Background(), s, disk, discardLogger())
	if err == nil {
		t.Fatal("expected divergence error when etcd holds a different active CA, got nil")
	}
	if s.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0 on divergence", s.createCalls)
	}
}

func TestBootstrapClusterCARaceLostMatchingWinner(t *testing.T) {
	ca := testCA(t)
	// Empty at first read, but CreateCACert loses the active-index race; the
	// winner persisted the same shared CA (joiners fetched it), so fingerprints
	// match and bootstrap succeeds.
	s := &fakeCABootstrapStore{
		createErr:  store.ErrCACertActiveExists,
		raceWinner: rowFromCA(ca),
	}
	if err := api.BootstrapClusterCA(context.Background(), s, ca, discardLogger()); err != nil {
		t.Fatalf("BootstrapClusterCA(race lost, matching winner): %v", err)
	}
}

func TestBootstrapClusterCARaceLostDivergentWinner(t *testing.T) {
	ca := testCA(t)
	other := testCA(t)
	s := &fakeCABootstrapStore{
		createErr:  store.ErrCACertActiveExists,
		raceWinner: rowFromCA(other),
	}
	if err := api.BootstrapClusterCA(context.Background(), s, ca, discardLogger()); err == nil {
		t.Fatal("expected error when the race winner persisted a divergent CA, got nil")
	}
}
