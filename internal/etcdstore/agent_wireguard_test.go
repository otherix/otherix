// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcdstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/store"
)

func TestAgentWireguardAllocation(t *testing.T) {
	ctx := context.Background()
	s, _ := startStore(t)
	nodeA := uuid.New()
	nodeB := uuid.New()
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: nodeA, PublicKey: "pkA", Endpoint: "a.example:51820", ListenPort: 51820}); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: nodeB, PublicKey: "pkB", Endpoint: "b.example:51820", ListenPort: 51820}); err != nil {
		t.Fatalf("upsert B: %v", err)
	}
	a, err := s.AgentWireguardByNodeID(ctx, nodeA)
	if err != nil {
		t.Fatalf("get A: %v", err)
	}
	b, err := s.AgentWireguardByNodeID(ctx, nodeB)
	if err != nil {
		t.Fatalf("get B: %v", err)
	}
	if a.AgentIndex == b.AgentIndex {
		t.Errorf("indices not distinct: A=%d B=%d", a.AgentIndex, b.AgentIndex)
	}
	if a.OverlayIP.String() != "10.42.0.1" {
		t.Errorf("A OverlayIP = %s, want 10.42.0.1", a.OverlayIP)
	}
	if b.OverlayIP.String() != "10.42.1.1" {
		t.Errorf("B OverlayIP = %s, want 10.42.1.1", b.OverlayIP)
	}
}

func TestAgentWireguardReReportKeepsAllocation(t *testing.T) {
	ctx := context.Background()
	s, _ := startStore(t)
	node := uuid.New()
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: node, PublicKey: "pk", Endpoint: "e1:51820", ListenPort: 51820}); err != nil {
		t.Fatalf("first: %v", err)
	}
	first, _ := s.AgentWireguardByNodeID(ctx, node)
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: node, PublicKey: "pk", Endpoint: "e2:51820", ListenPort: 51821}); err != nil {
		t.Fatalf("second: %v", err)
	}
	second, _ := s.AgentWireguardByNodeID(ctx, node)
	if second.AgentIndex != first.AgentIndex || second.OverlayIP != first.OverlayIP {
		t.Errorf("re-report changed allocation: %+v -> %+v", first, second)
	}
	if second.Endpoint != "e2:51820" || second.ListenPort != 51821 {
		t.Errorf("re-report did not refresh endpoint/port: %+v", second)
	}
}

func TestAgentWireguardPubkeyChangeSwapsGuard(t *testing.T) {
	ctx := context.Background()
	s, _ := startStore(t)
	node := uuid.New()
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: node, PublicKey: "old", Endpoint: "e:51820"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: node, PublicKey: "new", Endpoint: "e:51820"}); err != nil {
		t.Fatalf("change: %v", err)
	}
	rec, _ := s.AgentWireguardByNodeID(ctx, node)
	if rec.PublicKey != "new" {
		t.Errorf("pubkey = %s, want new", rec.PublicKey)
	}
	other := uuid.New()
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: other, PublicKey: "old", Endpoint: "o:51820"}); err != nil {
		t.Errorf("old pubkey not released: %v", err)
	}
}

func TestAgentWireguardPubkeyCollisionRejected(t *testing.T) {
	ctx := context.Background()
	s, _ := startStore(t)
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: uuid.New(), PublicKey: "dup", Endpoint: "a:51820"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: uuid.New(), PublicKey: "dup", Endpoint: "b:51820"})
	if !errors.Is(err, store.ErrAgentWireguardPubkeyInUse) {
		t.Errorf("collision err = %v, want ErrAgentWireguardPubkeyInUse", err)
	}
}

func TestAgentWireguardSupernetExhausted(t *testing.T) {
	ctx := context.Background()
	s, _ := startStore(t)
	if err := s.SeedOverlaySupernet(ctx, "10.99.0.0/24"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: uuid.New(), PublicKey: "p0", Endpoint: "a:51820"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: uuid.New(), PublicKey: "p1", Endpoint: "b:51820"})
	if !errors.Is(err, store.ErrOverlaySupernetExhausted) {
		t.Errorf("overflow err = %v, want ErrOverlaySupernetExhausted", err)
	}
}

func TestListAgentWireguard(t *testing.T) {
	ctx := context.Background()
	s, _ := startStore(t)
	for _, pk := range []string{"p1", "p2", "p3"} {
		if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: uuid.New(), PublicKey: pk, Endpoint: pk + ":51820"}); err != nil {
			t.Fatalf("upsert %s: %v", pk, err)
		}
	}
	all, err := s.ListAgentWireguard(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("len = %d, want 3", len(all))
	}
}

func TestAgentWireguardByNodeIDNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := startStore(t)
	_, err := s.AgentWireguardByNodeID(ctx, uuid.New())
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want store.ErrNotFound", err)
	}
}
