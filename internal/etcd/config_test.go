// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcd

import (
	"io"
	"log/slog"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	base := func() Config {
		return Config{
			Mode:         ModeSingle,
			Name:         "n1",
			DataDir:      "/tmp/x",
			PeerURL:      "http://127.0.0.1:12380",
			ClientURL:    "http://127.0.0.1:12379",
			ClusterToken: "otherix",
		}
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
		{name: "missing cluster-token", mutate: func(c *Config) { c.ClusterToken = "" }, wantErr: true},
		{name: "bootstrap needs initial-cluster", mutate: func(c *Config) { c.Mode = ModeBootstrap }, wantErr: true},
		{name: "join needs initial-cluster", mutate: func(c *Config) { c.Mode = ModeJoin }, wantErr: true},
		{
			name: "bootstrap with initial-cluster",
			mutate: func(c *Config) {
				c.Mode = ModeBootstrap
				c.InitialCluster = "n1=http://127.0.0.1:12380"
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
