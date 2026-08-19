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
	if b.OverlayIP.String() != "10.42.0.2" {
		t.Errorf("B OverlayIP = %s, want 10.42.0.2", b.OverlayIP)
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
	if err := s.SeedOverlaySupernet(ctx, "10.99.0.0/30"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// /30 has 2 usable hosts (.1, .2); the third allocation must exhaust.
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: uuid.New(), PublicKey: "p0", Endpoint: "a:51820"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: uuid.New(), PublicKey: "p1", Endpoint: "b:51820"}); err != nil {
		t.Fatalf("second: %v", err)
	}
	err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: uuid.New(), PublicKey: "p2", Endpoint: "c:51820"})
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

func TestDeleteNodePurgesAgentWireguard(t *testing.T) {
	ctx := context.Background()
	s, _ := startStore(t)
	a, err := s.CreateNode(ctx, nodeParams(uniqueNodeName("wg-a")))
	if err != nil {
		t.Fatalf("CreateNode A: %v", err)
	}
	b, err := s.CreateNode(ctx, nodeParams(uniqueNodeName("wg-b")))
	if err != nil {
		t.Fatalf("CreateNode B: %v", err)
	}
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: a.ID, PublicKey: "shared", Endpoint: "a:51820", ListenPort: 51820}); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if _, err := s.AgentWireguardByNodeID(ctx, a.ID); err != nil {
		t.Fatalf("AgentWireguardByNodeID(A) before delete: %v", err)
	}
	if _, err := s.DeleteNode(ctx, a.ID, false, uuid.New()); err != nil {
		t.Fatalf("DeleteNode(A): %v", err)
	}
	if _, err := s.AgentWireguardByNodeID(ctx, a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("AgentWireguardByNodeID(A) after delete = %v, want store.ErrNotFound", err)
	}
	// The pubkey guard must be released so node B can reclaim the same key.
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{NodeID: b.ID, PublicKey: "shared", Endpoint: "b:51820", ListenPort: 51820}); err != nil {
		t.Errorf("reclaim pubkey on B = %v, want nil (guard released)", err)
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

func TestAgentWireguardPreserveEstablishedPeers(t *testing.T) {
	ctx := context.Background()
	s, _ := startStore(t)
	node := uuid.New()
	peer := uuid.New().String()
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{
		NodeID: node, PublicKey: "pk", EstablishedPeers: []string{peer},
	}); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}
	// The agent could not observe its peer set this tick: every other field still
	// applies, the peer set is left alone.
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{
		NodeID: node, PublicKey: "pk", ListenPort: 51821,
		EstablishedPeers: nil, PreserveEstablishedPeers: true,
	}); err != nil {
		t.Fatalf("preserve upsert: %v", err)
	}
	rec, err := s.AgentWireguardByNodeID(ctx, node)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(rec.EstablishedPeers) != 1 || rec.EstablishedPeers[0] != peer {
		t.Errorf("EstablishedPeers = %v, want [%s] preserved", rec.EstablishedPeers, peer)
	}
	if rec.ListenPort != 51821 {
		t.Errorf("ListenPort = %d, want 51821 (other fields still apply)", rec.ListenPort)
	}
	// Without the flag an empty report is authoritative and clears the set.
	if err := s.UpsertAgentWireguard(ctx, store.UpsertAgentWireguardParams{
		NodeID: node, PublicKey: "pk", EstablishedPeers: nil,
	}); err != nil {
		t.Fatalf("clearing upsert: %v", err)
	}
	rec, err = s.AgentWireguardByNodeID(ctx, node)
	if err != nil {
		t.Fatalf("read back after clear: %v", err)
	}
	if len(rec.EstablishedPeers) != 0 {
		t.Errorf("EstablishedPeers = %v, want cleared by an authoritative empty report", rec.EstablishedPeers)
	}
}
