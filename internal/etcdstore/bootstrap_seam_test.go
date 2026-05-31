// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	apipkg "github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/etcdstore"
	"github.com/otherix/otherix/internal/store"
)

// The etcd store satisfies the boot-time bootstrap storage seams.
var (
	_ apipkg.AdminBootstrapStore     = (*etcdstore.Store)(nil)
	_ apipkg.ClusterCABootstrapStore = (*etcdstore.Store)(nil)
	_ apipkg.CPCertCAStore           = (*etcdstore.Store)(nil)
)

func TestCountAdmins(t *testing.T) {
	s, _ := startStore(t)
	ctx := context.Background()

	mkUser := func(email, role string) {
		if _, err := s.CreateUser(ctx, store.CreateUserParams{
			ID: uuid.New(), Email: email, PasswordHash: "x", Role: role,
		}); err != nil {
			t.Fatalf("CreateUser(%s): %v", email, err)
		}
	}

	if n, err := s.CountAdmins(ctx); err != nil || n != 0 {
		t.Fatalf("CountAdmins empty = (%d, %v), want 0", n, err)
	}
	mkUser("admin1@test", "admin")
	mkUser("dev@test", "developer")
	mkUser("admin2@test", "admin")
	if n, err := s.CountAdmins(ctx); err != nil || n != 2 {
		t.Errorf("CountAdmins = (%d, %v), want 2", n, err)
	}
}
