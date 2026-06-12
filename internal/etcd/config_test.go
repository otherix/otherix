// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	// freshDir is a data dir with no member subdir, so memberDirExists() is false
	// and the initial-cluster requirement applies to bootstrap / join.
	freshDir := t.TempDir()
	base := func() Config {
		return Config{
			Mode:         ModeSingle,
			Name:         "n1",
			DataDir:      freshDir,
			PeerURL:      "http://127.0.0.1:12380",
			ClientURL:    "http://127.0.0.1:12379",
			ClusterToken: "otherix",
		}
	}

	// initializedDir already has its member subdir (a member that has bootstrapped
	// and recovers membership from its WAL), so initial-cluster is not required.
	initializedDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(initializedDir, "member"), 0o755); err != nil {
		t.Fatalf("create member dir: %v", err)
	}
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{name: "valid single", mutate: func(*Config) {}, wantErr: false},
		{name: "bad mode", mutate: func(c *Config) { c.Mode = "wat" }, wantErr: true},
		{name: "missing name", mutate: func(c *Config) { c.Name = "" }, wantErr: true},
		{name: "missing data-dir", mutate: func(c *Config) { c.DataDir = "" }, wantErr: true},
		{name: "missing peer-url", mutate: func(c *Config) { c.PeerURL = "" }, wantErr: true},
		{name: "malformed peer-url", mutate: func(c *Config) { c.PeerURL = "://nope" }, wantErr: true},
		{name: "https peer without tls files", mutate: func(c *Config) { c.PeerURL = "https://127.0.0.1:12380" }, wantErr: true},
		{
			name: "https peer with tls files",
			mutate: func(c *Config) {
				c.PeerURL = "https://127.0.0.1:12380"
				c.PeerCertFile = "/x/peer.crt"
				c.PeerKeyFile = "/x/peer.key"
				c.PeerCAFile = "/x/ca.crt"
			},
			wantErr: false,
		},
		{name: "missing client-url", mutate: func(c *Config) { c.ClientURL = "" }, wantErr: true},
		{name: "client-url bind-all rejected", mutate: func(c *Config) { c.ClientURL = "http://0.0.0.0:2379" }, wantErr: true},
		{name: "client-url ipv6 bind-all rejected", mutate: func(c *Config) { c.ClientURL = "http://[::]:2379" }, wantErr: true},
		{name: "client-url private ip rejected", mutate: func(c *Config) { c.ClientURL = "http://10.0.0.5:2379" }, wantErr: true},
		{name: "client-url lan ip rejected", mutate: func(c *Config) { c.ClientURL = "http://192.168.1.1:2379" }, wantErr: true},
		{name: "client-url public host rejected", mutate: func(c *Config) { c.ClientURL = "https://etcd.example.com:2379" }, wantErr: true},
		// Intentional fail-closed: the guard pins on the literal address and does
		// no DNS resolution, so a non-"localhost" name is rejected even if it
		// would resolve to 127.0.0.1 via /etc/hosts.
		{name: "client-url loopback-resolving hostname rejected", mutate: func(c *Config) { c.ClientURL = "http://myhost.local:2379" }, wantErr: true},
		{name: "client-url userinfo trick rejected", mutate: func(c *Config) { c.ClientURL = "http://127.0.0.1@evil.com:2379" }, wantErr: true},
		{name: "client-url ipv4-mapped loopback ok", mutate: func(c *Config) { c.ClientURL = "http://[::ffff:127.0.0.1]:2379" }, wantErr: false},
		{name: "client-url ipv6 loopback ok", mutate: func(c *Config) { c.ClientURL = "http://[::1]:2379" }, wantErr: false},
		{name: "client-url localhost ok", mutate: func(c *Config) { c.ClientURL = "http://localhost:2379" }, wantErr: false},
		{name: "client-url localhost uppercase ok", mutate: func(c *Config) { c.ClientURL = "http://LOCALHOST:2379" }, wantErr: false},
		{name: "client-url 127.0.0.5 ok", mutate: func(c *Config) { c.ClientURL = "http://127.0.0.5:2379" }, wantErr: false},
		{name: "missing cluster-token", mutate: func(c *Config) { c.ClusterToken = "" }, wantErr: true},
		{name: "bootstrap needs initial-cluster", mutate: func(c *Config) { c.Mode = ModeBootstrap }, wantErr: true},
		{name: "join needs initial-cluster on fresh data dir", mutate: func(c *Config) { c.Mode = ModeJoin }, wantErr: true},
		{
			name: "bootstrap with initial-cluster",
			mutate: func(c *Config) {
				c.Mode = ModeBootstrap
				c.InitialCluster = "n1=http://127.0.0.1:12380"
			},
			wantErr: false,
		},
		{
			// A self-driven join node restarting: no initial-cluster, but the
			// member dir exists, so etcd recovers membership from the WAL.
			name: "join without initial-cluster on initialised member",
			mutate: func(c *Config) {
				c.Mode = ModeJoin
				c.DataDir = initializedDir
			},
			wantErr: false,
		},
		{
			// ModeSingle never needs initial-cluster, fresh data dir or not.
			name: "single unaffected by member dir requirement",
			mutate: func(c *Config) {
				c.Mode = ModeSingle
				c.DataDir = freshDir
			},
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(&c)
			err := c.Validate()
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Errorf("Validate() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.5", true},
		{"::1", true},
		{"::ffff:127.0.0.1", true},
		{"localhost", true},
		{"LocalHost", true},
		{"0.0.0.0", false},
		{"::", false},
		{"10.0.0.5", false},
		{"192.168.1.1", false},
		{"example.com", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := isLoopbackHost(tt.host); got != tt.want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestInitialClusterString(t *testing.T) {
	single := Config{Name: "n1", PeerURL: "http://127.0.0.1:12380"}
	if got, want := single.initialClusterString(), "n1=http://127.0.0.1:12380"; got != want {
		t.Errorf("initialClusterString() = %q, want %q (derived self entry)", got, want)
	}
	explicit := Config{Name: "n1", PeerURL: "x", InitialCluster: "n1=a,n2=b"}
	if got, want := explicit.initialClusterString(), "n1=a,n2=b"; got != want {
		t.Errorf("initialClusterString() = %q, want %q (explicit list)", got, want)
	}
}

func TestBuildEmbedConfigAutoCompaction(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	base := func() *Config {
		return &Config{
			Mode:         ModeSingle,
			Name:         "n1",
			DataDir:      t.TempDir(),
			PeerURL:      "http://127.0.0.1:12380",
			ClientURL:    "http://127.0.0.1:12379",
			ClusterToken: "otherix",
		}
	}

	t.Run("defaults bound MVCC history when unset", func(t *testing.T) {
		ec, err := buildEmbedConfig(base(), log)
		if err != nil {
			t.Fatalf("buildEmbedConfig: %v", err)
		}
		if ec.AutoCompactionMode != "periodic" {
			t.Errorf("AutoCompactionMode = %q, want periodic", ec.AutoCompactionMode)
		}
		if ec.AutoCompactionRetention != "1h" {
			t.Errorf("AutoCompactionRetention = %q, want 1h (non-zero, so history is bounded)", ec.AutoCompactionRetention)
		}
	})

	t.Run("explicit values pass through", func(t *testing.T) {
		c := base()
		c.CompactionMode = "revision"
		c.CompactionRetention = "5000"
		ec, err := buildEmbedConfig(c, log)
		if err != nil {
			t.Fatalf("buildEmbedConfig: %v", err)
		}
		if ec.AutoCompactionMode != "revision" || ec.AutoCompactionRetention != "5000" {
			t.Errorf("auto-compaction = %q/%q, want revision/5000",
				ec.AutoCompactionMode, ec.AutoCompactionRetention)
		}
	})
}
