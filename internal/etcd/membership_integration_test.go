// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build integration
// +build integration

package etcd_test

import (
	"context"
	"crypto"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcd"
)

// learnerCount returns the number of learner members in the slice.
func learnerCount(members []etcd.Member) int {
	n := 0
	for _, m := range members {
		if m.IsLearner {
			n++
		}
	}
	return n
}

// TestMembershipClient exercises MembershipClient against a real single-node
// embedded member: ListMembers, RegisterLearner (including idempotency), and
// TryPromoteLearners (best-effort, no-op on a never-started learner).
func TestMembershipClient(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ca, err := auth.GenerateClusterCA(time.Now())
	if err != nil {
		t.Fatalf("GenerateClusterCA: %v", err)
	}
	caKeyAny, err := auth.ParseClusterCAKey(ca.KeyPEM)
	if err != nil {
		t.Fatalf("ParseClusterCAKey: %v", err)
	}
	signer, ok := caKeyAny.(crypto.Signer)
	if !ok {
		t.Fatal("CA key is not a crypto.Signer")
	}

	peer0 := fmt.Sprintf("https://127.0.0.1:%d", freePort(t))
	client0 := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	peer1 := fmt.Sprintf("https://127.0.0.1:%d", freePort(t))

	cert0, key0, caf0 := peerMaterial(t, dir, "n0", signer, ca)

	cfg0 := &etcd.Config{
		Mode:         etcd.ModeSingle,
		Name:         "n0",
		DataDir:      filepath.Join(dir, "n0"),
		PeerURL:      peer0,
		ClientURL:    client0,
		ClusterToken: "otherix-membership",
		PeerCertFile: cert0,
		PeerKeyFile:  key0,
		PeerCAFile:   caf0,
	}
	r0, err := etcd.Start(ctx, cfg0, log)
	if err != nil {
		t.Fatalf("start n0: %v", err)
	}
	defer r0.Stop(10 * time.Second)

	mc := etcd.NewMembershipClient(client0, log)

	// ListMembers on a fresh single-node cluster: exactly one voter.
	members, err := mc.ListMembers(ctx)
	if err != nil {
		t.Fatalf("ListMembers: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("ListMembers count = %d, want 1", len(members))
	}
	if members[0].IsLearner {
		t.Errorf("single-node member is a learner, want voter")
	}

	// RegisterLearner: a phantom peer URL not backed by a running member.
	after, err := mc.RegisterLearner(ctx, peer1)
	if err != nil {
		t.Fatalf("RegisterLearner: %v", err)
	}
	if lc := learnerCount(after); lc != 1 {
		t.Errorf("learnerCount after RegisterLearner = %d, want 1", lc)
	}
	totalAfter := len(after)

	// RegisterLearner again with the same peer URL - idempotent, no duplicate.
	after2, err := mc.RegisterLearner(ctx, peer1)
	if err != nil {
		t.Fatalf("RegisterLearner (idempotent): %v", err)
	}
	if len(after2) != totalAfter {
		t.Errorf("member count after idempotent RegisterLearner = %d, want %d", len(after2), totalAfter)
	}

	// TryPromoteLearners: the phantom learner has never started and cannot have
	// caught up, so promotion is rejected by etcd. Expect 0 promoted, nil error.
	promoted, err := mc.TryPromoteLearners(ctx)
	if err != nil {
		t.Fatalf("TryPromoteLearners: %v", err)
	}
	if promoted != 0 {
		t.Errorf("TryPromoteLearners promoted = %d, want 0 (learner never started)", promoted)
	}
}
