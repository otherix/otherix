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
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcd"
)

// peerMaterial writes a peer cert/key (signed by ca) plus the CA trust file for
// one member into dir, returning the three paths. SAN covers 127.0.0.1 so both
// loopback members validate each other.
func peerMaterial(t *testing.T, dir, prefix string, signer crypto.Signer, ca auth.ClusterCAResult) (certFile, keyFile, caFile string) {
	t.Helper()
	parsedCA, _, err := auth.ParseClusterCACert(ca.CertPEM)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	certPEM, keyPEM, err := auth.GeneratePeerCert(parsedCA, signer,
		[]string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, auth.PeerCertValidity, time.Now())
	if err != nil {
		t.Fatalf("GeneratePeerCert(%s): %v", prefix, err)
	}
	certFile = filepath.Join(dir, prefix+"-peer.crt")
	keyFile = filepath.Join(dir, prefix+"-peer.key")
	caFile = filepath.Join(dir, prefix+"-ca.crt")
	writeFile(t, certFile, certPEM)
	writeFile(t, keyFile, keyPEM)
	writeFile(t, caFile, ca.CertPEM)
	return certFile, keyFile, caFile
}

// TestSingleToHATransition grows a single-node member to a 2-voter cluster over
// peer mTLS: start node0 single, add node1 as a learner, start node1 in join
// mode, promote it, and confirm both serve as voters and replicate. This ties
// the 9c membership primitives to the 9d peer-PKI and drives the MembershipClient
// promote path end to end.
func TestSingleToHATransition(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Shared cluster CA + a signer parsed once.
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

	// Allocate the four loopback ports up front.
	peer0 := fmt.Sprintf("https://127.0.0.1:%d", freePort(t))
	client0 := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	peer1 := fmt.Sprintf("https://127.0.0.1:%d", freePort(t))
	client1 := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))

	cert0, key0, caf0 := peerMaterial(t, dir, "n0", signer, ca)
	cert1, key1, caf1 := peerMaterial(t, dir, "n1", signer, ca)

	// node0: single-node member with peer mTLS.
	cfg0 := &etcd.Config{
		Mode:         etcd.ModeSingle,
		Name:         "n0",
		DataDir:      filepath.Join(dir, "n0"),
		PeerURL:      peer0,
		ClientURL:    client0,
		ClusterToken: "otherix-transition",
		PeerCertFile: cert0,
		PeerKeyFile:  key0,
		PeerCAFile:   caf0,
	}
	r0, err := etcd.Start(ctx, cfg0, log)
	if err != nil {
		t.Fatalf("start node0: %v", err)
	}
	defer r0.Stop(10 * time.Second)

	mc0 := etcd.NewMembershipClient(client0, log)
	if n, err := mc0.VoterCount(ctx); err != nil || n != 1 {
		t.Fatalf("VoterCount(single) = (%d, %v), want 1", n, err)
	}

	// Add node1 as a learner via node0, then start node1 in join mode.
	if _, err := mc0.RegisterLearner(ctx, peer1); err != nil {
		t.Fatalf("RegisterLearner: %v", err)
	}

	cfg1 := &etcd.Config{
		Mode:           etcd.ModeJoin,
		Name:           "n1",
		DataDir:        filepath.Join(dir, "n1"),
		PeerURL:        peer1,
		ClientURL:      client1,
		ClusterToken:   "otherix-transition",
		InitialCluster: fmt.Sprintf("n0=%s,n1=%s", peer0, peer1),
		PeerCertFile:   cert1,
		PeerKeyFile:    key1,
		PeerCAFile:     caf1,
	}
	r1, err := etcd.Start(ctx, cfg1, log)
	if err != nil {
		t.Fatalf("start node1 (join): %v", err)
	}
	defer r1.Stop(10 * time.Second)

	// Promote the learner to a voter by retrying the single-shot promote until
	// the cluster reports 2 voters - etcd rejects promotion until the learner's
	// raft log has caught up, so the loop is the catch-up wait. This mirrors the
	// production transition watcher, which polls TryPromoteLearners the same way.
	promoteDeadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := mc0.TryPromoteLearners(ctx); err != nil {
			t.Fatalf("TryPromoteLearners: %v", err)
		}
		n, err := mc0.VoterCount(ctx)
		if err != nil {
			t.Fatalf("VoterCount(during promote): %v", err)
		}
		if n == 2 {
			break
		}
		if time.Now().After(promoteDeadline) {
			t.Fatalf("learner not promoted to voter within deadline: VoterCount = %d, want 2", n)
		}
		time.Sleep(500 * time.Millisecond)
	}

	mc1 := etcd.NewMembershipClient(client1, log)
	if err := mc1.WaitServing(ctx); err != nil {
		t.Fatalf("WaitServing(node1): %v", err)
	}

	if n, err := mc0.VoterCount(ctx); err != nil || n != 2 {
		t.Fatalf("VoterCount(after promote) = (%d, %v), want 2", n, err)
	}

	// Replication: a write on node0 is observable on node1.
	const key, want = "/otherix/test/transition", "replicated"
	if err := put(ctx, client0, key, want); err != nil {
		t.Fatalf("put on node0: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, gerr := get(ctx, client1, key)
		if gerr == nil && got == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("key not replicated to node1 within deadline: got %q err %v", got, gerr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
