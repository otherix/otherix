// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

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
	cfgPath := seedAPIYAML(t, "etcd:\n  mode: join\n  name: otherix-1\n")
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
func TestRunJoinIdempotentWhenFileFailsStandaloneValidate(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "api.yaml")
	// No auth.jwt_secret on purpose: config.LoadAPI(cfgPath) returns an error.
	content := "server:\n  listen: \"0.0.0.0:9090\"\netcd:\n  mode: join\n  name: otherix-1\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatalf("seed api.yaml: %v", err)
	}
	if _, err := config.LoadAPI(cfgPath); err == nil {
		t.Fatalf("precondition: expected LoadAPI to fail standalone Validate, got nil")
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
		t.Errorf("restarted = true, want false (joined node must no-op even when the file fails standalone Validate)")
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
