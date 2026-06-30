// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

func TestSSHUserCA_CreateIdempotentAndActive(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	if _, err := s.ActiveSSHUserCA(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ActiveSSHUserCA on empty store error = %v, want ErrNotFound", err)
	}
	mat, err := auth.GenerateSSHUserCA()
	if err != nil {
		t.Fatalf("GenerateSSHUserCA() error = %v", err)
	}
	row, err := s.CreateSSHUserCA(ctx, store.CreateSSHUserCAParams{
		PrivateKeyPEM: mat.PrivateKeyPEM, PublicKeyAuthorized: mat.PublicKeyAuthorized,
	})
	if err != nil {
		t.Fatalf("CreateSSHUserCA() error = %v", err)
	}
	// Second create loses the race and returns the SAME row.
	mat2, _ := auth.GenerateSSHUserCA()
	row2, err := s.CreateSSHUserCA(ctx, store.CreateSSHUserCAParams{
		PrivateKeyPEM: mat2.PrivateKeyPEM, PublicKeyAuthorized: mat2.PublicKeyAuthorized,
	})
	if err != nil {
		t.Fatalf("CreateSSHUserCA() second error = %v", err)
	}
	if row2.ID != row.ID {
		t.Errorf("second CreateSSHUserCA id = %v, want winner %v", row2.ID, row.ID)
	}
	active, err := s.ActiveSSHUserCA(ctx)
	if err != nil {
		t.Fatalf("ActiveSSHUserCA() error = %v", err)
	}
	if active.ID != row.ID {
		t.Errorf("active id = %v, want %v", active.ID, row.ID)
	}
}
