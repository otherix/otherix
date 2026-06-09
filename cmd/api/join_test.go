// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/internal/config"
)

// fp64 is a 64-char lowercase-hex stand-in for a cluster CA sha256
// fingerprint, shared across the join tests.
const fp64 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// seedAPIYAML writes a minimal api.yaml with a server block and the given
// etcd block to a temp dir, returning its path.
func seedAPIYAML(t *testing.T, etcdBlock string) string {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "api.yaml")
	content := "server:\n" +
		"  listen: \"0.0.0.0:9090\"\n" +
		"auth:\n" +
		"  jwt_secret: \"0123456789abcdef0123456789abcdef\"\n" +
		etcdBlock
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("seed api.yaml: %v", err)
	}
	return cfgPath
}

func TestWriteJoinConfigSetsModeAndBlock(t *testing.T) {
	cfgPath := seedAPIYAML(t, "etcd:\n  mode: single\n  name: otherix-0\n  peer_url: auto\n")
	tokenPath := filepath.Join(t.TempDir(), "cluster-join-token")

	in := joinInputs{
		cpURL:         "https://cp.example:8443",
		caFingerprint: "sha256:" + fp64,
		name:          "otherix-1",
		configPath:    cfgPath,
		tokenPath:     tokenPath,
	}
	if err := writeJoinConfig(in); err != nil {
		t.Fatalf("writeJoinConfig: %v", err)
	}

	cfg, err := config.LoadAPI(cfgPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.Etcd.Mode != "join" {
		t.Errorf("Etcd.Mode = %q, want %q", cfg.Etcd.Mode, "join")
	}
	if cfg.Etcd.Name != "otherix-1" {
		t.Errorf("Etcd.Name = %q, want %q", cfg.Etcd.Name, "otherix-1")
	}
	if cfg.ClusterJoin.CPURL != "https://cp.example:8443" {
		t.Errorf("ClusterJoin.CPURL = %q, want %q", cfg.ClusterJoin.CPURL, "https://cp.example:8443")
	}
	if cfg.ClusterJoin.TokenPath != tokenPath {
		t.Errorf("ClusterJoin.TokenPath = %q, want %q", cfg.ClusterJoin.TokenPath, tokenPath)
	}
	if cfg.ClusterJoin.CAFingerprint == "" {
		t.Errorf("ClusterJoin.CAFingerprint is empty, want non-empty")
	}
	if cfg.Server.Listen != "0.0.0.0:9090" {
		t.Errorf("Server.Listen = %q, want %q (original key must survive rewrite)", cfg.Server.Listen, "0.0.0.0:9090")
	}
}

func TestRunJoinIdempotentWhenAlreadyJoined(t *testing.T) {
	dir := t.TempDir()
	caCert := filepath.Join(dir, "cluster-ca.crt")
	caKey := filepath.Join(dir, "cluster-ca.key")
	cfgPath := seedAPIYAML(t,
		"cluster_ca:\n  cert_file: \""+caCert+"\"\n  key_file: \""+caKey+"\"\n"+
			"etcd:\n  mode: join\n  name: otherix-1\n")
	// Completed join: cluster CA cert+key present on disk.
	if err := os.WriteFile(caCert, []byte("dummy-cert"), 0o644); err != nil {
		t.Fatalf("seed ca cert: %v", err)
	}
	if err := os.WriteFile(caKey, []byte("dummy-key"), 0o600); err != nil {
		t.Fatalf("seed ca key: %v", err)
	}
	tokenDest := filepath.Join(t.TempDir(), "cluster-join-token")

	restarted := false
	orig := restartUnit
	restartUnit = func(*slog.Logger) error {
		restarted = true
		return nil
	}
	defer func() { restartUnit = orig }()

	cmd := newJoinCommand()
	cmd.SetArgs([]string{
		"--cp-url", "https://cp.example:8443",
		"--ca-fingerprint", "sha256:" + fp64,
		"--token", "t",
		"--config", cfgPath,
		"--token-dest", tokenDest,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("join Execute: %v", err)
	}
	if restarted {
		t.Errorf("restarted = true, want false (already-joined must not restart without --force)")
	}
}

// TestRunJoinIdempotentWhenFileFailsStandaloneValidate locks the seam fix: the
// idempotency guard must read etcd.mode with bare koanf, not config.LoadAPI. A
// joined replica whose api.yaml omits jwt_secret (supplied via env at serve
// time - a common HA shape) fails standalone Validate; the old LoadAPI-based
// guard would misread that as "not joined" and rewrite + restart. The fix must
// still no-op here.
//
// This case also represents a HEALTHY joined node (Rel-I1): the no-op now
// requires the cluster CA cert+key to be present on disk (evidence the join
// actually completed), so the test seeds dummy CA files at the configured
// cluster_ca paths in addition to mode=join.
func TestRunJoinIdempotentWhenFileFailsStandaloneValidate(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "api.yaml")
	caCert := filepath.Join(dir, "cluster-ca.crt")
	caKey := filepath.Join(dir, "cluster-ca.key")
	// No auth.jwt_secret on purpose: config.LoadAPI(cfgPath) returns an error.
	// cluster_ca paths point at the dummy CA files seeded below, so the guard
	// sees a completed join.
	content := "server:\n  listen: \"0.0.0.0:9090\"\n" +
		"cluster_ca:\n  cert_file: \"" + caCert + "\"\n  key_file: \"" + caKey + "\"\n" +
		"etcd:\n  mode: join\n  name: otherix-1\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("seed api.yaml: %v", err)
	}
	if _, err := config.LoadAPI(cfgPath); err == nil {
		t.Fatalf("precondition: expected LoadAPI to fail standalone Validate, got nil")
	}
	// Completed join: cluster CA cert+key are present on disk.
	if err := os.WriteFile(caCert, []byte("dummy-cert"), 0o644); err != nil {
		t.Fatalf("seed ca cert: %v", err)
	}
	if err := os.WriteFile(caKey, []byte("dummy-key"), 0o600); err != nil {
		t.Fatalf("seed ca key: %v", err)
	}

	restarted := false
	orig := restartUnit
	restartUnit = func(*slog.Logger) error {
		restarted = true
		return nil
	}
	defer func() { restartUnit = orig }()

	cmd := newJoinCommand()
	cmd.SetArgs([]string{
		"--cp-url", "https://cp.example:8443",
		"--ca-fingerprint", "sha256:" + fp64,
		"--token", "t",
		"--config", cfgPath,
		"--token-dest", filepath.Join(dir, "cluster-join-token"),
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("join Execute: %v", err)
	}
	if restarted {
		t.Errorf("restarted = true, want false (healthy joined node must no-op even when the file fails standalone Validate)")
	}
}

// TestRunJoinReappliesWhenJoinIncomplete locks the Rel-I1 self-heal: a host
// whose api.yaml says mode=join but whose cluster CA is NOT on disk represents a
// FAILED / partial first join (config written, redemption never completed). A
// plain re-run with a corrected token must fall through and restart so the new
// token takes effect - it must NOT no-op on the stale mode=join flag.
func TestRunJoinReappliesWhenJoinIncomplete(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "api.yaml")
	caCert := filepath.Join(dir, "cluster-ca.crt")
	caKey := filepath.Join(dir, "cluster-ca.key")
	// mode=join but NO CA files on disk -> a partial/failed join.
	content := "server:\n  listen: \"0.0.0.0:9090\"\n" +
		"auth:\n  jwt_secret: \"0123456789abcdef0123456789abcdef\"\n" +
		"cluster_ca:\n  cert_file: \"" + caCert + "\"\n  key_file: \"" + caKey + "\"\n" +
		"etcd:\n  mode: join\n  name: otherix-1\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("seed api.yaml: %v", err)
	}

	restarted := false
	orig := restartUnit
	restartUnit = func(*slog.Logger) error {
		restarted = true
		return nil
	}
	defer func() { restartUnit = orig }()

	cmd := newJoinCommand()
	cmd.SetArgs([]string{
		"--cp-url", "https://cp.example:8443",
		"--ca-fingerprint", "sha256:" + fp64,
		"--token", "corrected-token",
		"--config", cfgPath,
		"--token-dest", filepath.Join(dir, "cluster-join-token"),
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("join Execute: %v", err)
	}
	if !restarted {
		t.Errorf("restarted = false, want true (failed/partial join must re-apply the corrected token and restart)")
	}
}

// TestWriteJoinConfigPreservesTypedKeys locks the round-trip invariant that the
// koanf load -> Set 5 join keys -> Marshal rewrite in writeJoinConfig does not
// silently change the TYPE, VALUE, or ORDER of unrelated keys the operator had
// set. koanf's yaml.v3 marshaler is quote-aware and these survive today; this
// test guards that against a future koanf / yaml bump. It seeds the fragile key
// shapes (a duration, a numeric-string, a bool, an order-sensitive list, and a
// string that would re-parse as a bool unquoted), runs the rewrite, reloads via
// config.LoadAPI, and asserts each survives intact.
func TestWriteJoinConfigPreservesTypedKeys(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "api.yaml")
	// A realistic api.yaml: valid enough for config.LoadAPI to succeed
	// (jwt_secret >= 32 bytes) and carrying the fragile typed keys.
	//   - auth.jwt_access_ttl: a time.Duration ("15m").
	//   - etcd.compaction_retention: a Go string field holding a numeric string
	//     ("5000") - must NOT coerce to a number on round-trip.
	//   - workers.enabled: a bool set to false (default is true, so the seed
	//     value is meaningful) - must NOT flip or drop.
	//   - storage_pools.allowed_path_prefixes: an order-sensitive list (index 0
	//     is the default-pool path) - order must survive.
	//   - etcd.cluster_token: "yes", a string the rewrite does NOT touch that
	//     would re-parse as a bool if unquoted - must stay the string "yes".
	content := "server:\n" +
		"  listen: \"0.0.0.0:9090\"\n" +
		"auth:\n" +
		"  jwt_secret: \"0123456789abcdef0123456789abcdef\"\n" +
		"  jwt_access_ttl: 15m\n" +
		"workers:\n" +
		"  enabled: false\n" +
		"storage_pools:\n" +
		"  allowed_path_prefixes: [\"/var/lib/otherix/pools/\", \"/mnt/extra/\"]\n" +
		"etcd:\n" +
		"  mode: single\n" +
		"  name: otherix-0\n" +
		"  peer_url: auto\n" +
		"  cluster_token: \"yes\"\n" +
		"  compaction_retention: \"5000\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("seed api.yaml: %v", err)
	}

	// Sanity: the seed loads, so the preconditions are valid.
	if _, err := config.LoadAPI(cfgPath); err != nil {
		t.Fatalf("precondition: seed must load, got error: %v", err)
	}

	tokenPath := filepath.Join(t.TempDir(), "cluster-join-token")
	in := joinInputs{
		cpURL:         "https://cp.example:8443",
		caFingerprint: "sha256:" + fp64,
		name:          "otherix-1",
		configPath:    cfgPath,
		tokenPath:     tokenPath,
	}
	if err := writeJoinConfig(in); err != nil {
		t.Fatalf("writeJoinConfig: %v", err)
	}

	cfg, err := config.LoadAPI(cfgPath)
	if err != nil {
		t.Fatalf("reload config after rewrite: %v", err)
	}

	// The 5 rewritten keys are set.
	if cfg.Etcd.Mode != "join" {
		t.Errorf("Etcd.Mode = %q, want %q", cfg.Etcd.Mode, "join")
	}
	if cfg.Etcd.Name != "otherix-1" {
		t.Errorf("Etcd.Name = %q, want %q", cfg.Etcd.Name, "otherix-1")
	}
	if cfg.ClusterJoin.CPURL != "https://cp.example:8443" {
		t.Errorf("ClusterJoin.CPURL = %q, want %q", cfg.ClusterJoin.CPURL, "https://cp.example:8443")
	}
	if cfg.ClusterJoin.TokenPath != tokenPath {
		t.Errorf("ClusterJoin.TokenPath = %q, want %q", cfg.ClusterJoin.TokenPath, tokenPath)
	}
	if cfg.ClusterJoin.CAFingerprint != "sha256:"+fp64 {
		t.Errorf("ClusterJoin.CAFingerprint = %q, want %q", cfg.ClusterJoin.CAFingerprint, "sha256:"+fp64)
	}

	// Duration survives as 15m.
	if cfg.Auth.JWTAccessTTL != 15*time.Minute {
		t.Errorf("Auth.JWTAccessTTL = %v, want %v", cfg.Auth.JWTAccessTTL, 15*time.Minute)
	}
	// Numeric-string string field survives as the string "5000", not a number.
	if cfg.Etcd.CompactionRetention != "5000" {
		t.Errorf("Etcd.CompactionRetention = %q, want %q", cfg.Etcd.CompactionRetention, "5000")
	}
	// Bool survives as false (default is true, so this proves the seed value
	// round-tripped rather than the default being reapplied).
	if cfg.Workers.Enabled {
		t.Errorf("Workers.Enabled = %v, want %v", cfg.Workers.Enabled, false)
	}
	// Quote-sensitive string survives as "yes", not the bool true.
	if cfg.Etcd.ClusterToken != "yes" {
		t.Errorf("Etcd.ClusterToken = %q, want %q", cfg.Etcd.ClusterToken, "yes")
	}
	// Order-sensitive list survives in seed order (index 0 is the default-pool
	// path, so order is load-bearing).
	wantPrefixes := []string{"/var/lib/otherix/pools/", "/mnt/extra/"}
	if diff := cmp.Diff(wantPrefixes, cfg.StoragePools.AllowedPathPrefixes); diff != "" {
		t.Errorf("StoragePools.AllowedPathPrefixes mismatch (-want +got):\n%s", diff)
	}
}

func TestRunJoinRestartsViaSeam(t *testing.T) {
	cfgPath := seedAPIYAML(t, "etcd:\n  mode: single\n  name: otherix-0\n  peer_url: auto\n")
	tokenDest := filepath.Join(t.TempDir(), "cluster-join-token")

	restarted := false
	orig := restartUnit
	restartUnit = func(*slog.Logger) error {
		restarted = true
		return nil
	}
	defer func() { restartUnit = orig }()

	cmd := newJoinCommand()
	cmd.SetArgs([]string{
		"--cp-url", "https://cp.example:8443",
		"--ca-fingerprint", "sha256:" + fp64,
		"--token", "t",
		"--name", "otherix-2",
		"--config", cfgPath,
		"--token-dest", tokenDest,
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("join Execute: %v", err)
	}
	if !restarted {
		t.Errorf("restarted = false, want true")
	}
}
