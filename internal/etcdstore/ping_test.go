// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"testing"

	healthhandler "github.com/otherix/otherix/internal/api/health"
	"github.com/otherix/otherix/internal/etcdstore"
)

// The etcd store satisfies the readiness Pinger.
var _ healthhandler.Pinger = (*etcdstore.Store)(nil)

func TestPing(t *testing.T) {
	s, _ := startStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Errorf("Ping = %v, want nil on a live member", err)
	}
}
