// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/otherix/otherix/internal/api"
	"github.com/otherix/otherix/internal/config"
)

// TestNameCollision covers the etcd.name collision check applied after the join
// fetch: a same-name member with a DIFFERENT peer URL is a real collision; a
// same-name member with the same peer URL (canonically, ignoring a trailing
// slash) is this node re-registering and is not a collision.
func TestNameCollision(t *testing.T) {
	cases := []struct {
		name        string
		members     []api.ClusterMemberRef
		selfName    string
		selfPeerURL string
		wantPeer    string
		wantCollide bool
	}{
		{
			name:        "same name different peer collides",
			members:     []api.ClusterMemberRef{{Name: "otherix-1", PeerURL: "https://10.0.0.9:2380"}},
			selfName:    "otherix-1",
			selfPeerURL: "https://10.0.0.2:2380",
			wantPeer:    "https://10.0.0.9:2380",
			wantCollide: true,
		},
		{
			name:        "same name same peer is self re-register",
			members:     []api.ClusterMemberRef{{Name: "otherix-1", PeerURL: "https://10.0.0.2:2380"}},
			selfName:    "otherix-1",
			selfPeerURL: "https://10.0.0.2:2380",
			wantCollide: false,
		},
		{
			name:        "same name same peer trailing slash is self re-register",
			members:     []api.ClusterMemberRef{{Name: "otherix-1", PeerURL: "https://10.0.0.2:2380/"}},
			selfName:    "otherix-1",
			selfPeerURL: "https://10.0.0.2:2380",
			wantCollide: false,
		},
		{
			name:        "distinct names do not collide",
			members:     []api.ClusterMemberRef{{Name: "otherix-0", PeerURL: "https://10.0.0.1:2380"}},
			selfName:    "otherix-1",
			selfPeerURL: "https://10.0.0.2:2380",
			wantCollide: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPeer, gotCollide := nameCollision(tc.members, tc.selfName, tc.selfPeerURL)
			if gotCollide != tc.wantCollide {
				t.Errorf("nameCollision collide = %v, want %v", gotCollide, tc.wantCollide)
			}
			if gotCollide && gotPeer != tc.wantPeer {
				t.Errorf("nameCollision peer = %q, want %q", gotPeer, tc.wantPeer)
			}
		})
	}
}

// joinTestConfig builds an APIConfig wired for a join boot against a temp dir.
// The caller layers on the disk fixtures (member dir, CA files, sidecar) the
// branch under test requires.
func joinTestConfig(t *testing.T) *config.APIConfig {
	t.Helper()
	root := t.TempDir()
	caDir := filepath.Join(root, "ca")
	if err := os.MkdirAll(caDir, 0o700); err != nil {
		t.Fatalf("mkdir ca dir: %v", err)
	}
	cfg := &config.APIConfig{}
	cfg.Etcd.Mode = "join"
	cfg.Etcd.Name = "otherix-1"
	cfg.Etcd.DataDir = filepath.Join(root, "etcd")
	cfg.Etcd.PeerURL = "https://10.0.0.2:2380"
	cfg.ClusterCA.CertFile = filepath.Join(caDir, "cluster-ca.crt")
	cfg.ClusterCA.KeyFile = filepath.Join(caDir, "cluster-ca.key")
	return cfg
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// TestEnsureClusterCAForJoin_MemberInitialized proves that once the etcd member
// dir exists the join path short-circuits to WAL recovery: it returns the
// configured initial-cluster verbatim, never touches the network, and does not
// require any CA or sidecar on disk. If this branch attempted a fetch the test
// would hang or fail (CPURL is empty, so no fetch can succeed).
func TestEnsureClusterCAForJoin_MemberInitialized(t *testing.T) {
	cfg := joinTestConfig(t)
	cfg.Etcd.InitialCluster = "sentinel=https://10.0.0.1:2380"
	if err := os.MkdirAll(filepath.Join(cfg.Etcd.DataDir, "member"), 0o700); err != nil {
		t.Fatalf("mkdir member: %v", err)
	}

	got, err := ensureClusterCAForJoin(context.Background(), cfg, quietLogger())
	if err != nil {
		t.Fatalf("ensureClusterCAForJoin returned error: %v", err)
	}
	if got != cfg.Etcd.InitialCluster {
		t.Errorf("initial-cluster = %q, want %q (the configured value, recovered from WAL)", got, cfg.Etcd.InitialCluster)
	}
}

// TestEnsureClusterCAForJoin_PartialJoinWithSidecar proves the crash-loop fix:
// CA on disk + no member dir + a saved sidecar resumes from the persisted
// initial-cluster instead of re-redeeming the spent token. The returned value is
// the trimmed sidecar contents; no fetch occurs.
func TestEnsureClusterCAForJoin_PartialJoinWithSidecar(t *testing.T) {
	cfg := joinTestConfig(t)
	writeFile(t, cfg.ClusterCA.CertFile, "ca-cert")
	writeFile(t, cfg.ClusterCA.KeyFile, "ca-key")
	wantIC := "n0=https://10.0.0.1:2380,otherix-1=https://10.0.0.2:2380"
	sidecar := filepath.Join(filepath.Dir(cfg.ClusterCA.CertFile), "initial-cluster")
	writeFile(t, sidecar, "  "+wantIC+"\n")

	got, err := ensureClusterCAForJoin(context.Background(), cfg, quietLogger())
	if err != nil {
		t.Fatalf("ensureClusterCAForJoin returned error: %v", err)
	}
	if got != wantIC {
		t.Errorf("initial-cluster = %q, want %q (trimmed sidecar contents)", got, wantIC)
	}
}

// TestEnsureClusterCAForJoin_PartialJoinWithoutSidecar proves that a partial
// join with no recoverable initial-cluster fails with a CLEAR actionable error
// (not the misleading etcd "initial-cluster is required") and never attempts a
// fetch with the already-spent token.
func TestEnsureClusterCAForJoin_PartialJoinWithoutSidecar(t *testing.T) {
	cfg := joinTestConfig(t)
	writeFile(t, cfg.ClusterCA.CertFile, "ca-cert")
	writeFile(t, cfg.ClusterCA.KeyFile, "ca-key")

	_, err := ensureClusterCAForJoin(context.Background(), cfg, quietLogger())
	if err == nil {
		t.Fatalf("ensureClusterCAForJoin returned nil error, want a clear partial-join error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "partial join") {
		t.Errorf("error %q does not mention %q", msg, "partial join")
	}
	if !strings.Contains(msg, cfg.Etcd.DataDir) {
		t.Errorf("error %q does not name the data dir %q", msg, cfg.Etcd.DataDir)
	}
}

// TestEnsureClusterCAForJoin_EmptySidecarTreatedAsMissing proves a present but
// empty sidecar (e.g. a truncated write) is treated as missing - it falls
// through to the clear actionable error rather than returning an empty
// initial-cluster that etcd would reject with the misleading message.
func TestEnsureClusterCAForJoin_EmptySidecarTreatedAsMissing(t *testing.T) {
	cfg := joinTestConfig(t)
	writeFile(t, cfg.ClusterCA.CertFile, "ca-cert")
	writeFile(t, cfg.ClusterCA.KeyFile, "ca-key")
	sidecar := filepath.Join(filepath.Dir(cfg.ClusterCA.CertFile), "initial-cluster")
	writeFile(t, sidecar, "   \n")

	_, err := ensureClusterCAForJoin(context.Background(), cfg, quietLogger())
	if err == nil {
		t.Fatalf("ensureClusterCAForJoin returned nil error for an empty sidecar, want the partial-join error")
	}
	if !strings.Contains(err.Error(), "partial join") {
		t.Errorf("error %q does not mention %q", err.Error(), "partial join")
	}
}
