// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadAPIFileAndDefaults(t *testing.T) {
	cfg, err := LoadAPI("testdata/api.yaml")
	if err != nil {
		t.Fatalf("LoadAPI(testdata/api.yaml) = %v, want no error", err)
	}

	if got, want := cfg.Server.Listen, "127.0.0.1:18080"; got != want {
		t.Errorf("Server.Listen = %q, want %q", got, want)
	}
	if got, want := cfg.Server.ReadTimeout, 30*time.Second; got != want {
		t.Errorf("Server.ReadTimeout = %v, want %v (default preserved)", got, want)
	}
	if got, want := cfg.Auth.JWTSecret, "from-file-padded-to-32-byte-min!"; got != want {
		t.Errorf("Auth.JWTSecret = %q, want %q", got, want)
	}
	if got, want := cfg.Console.AccessMode, "direct"; got != want {
		t.Errorf("Console.AccessMode = %q, want %q", got, want)
	}
	if got, want := cfg.Workers.MaxWorkers, 10; got != want {
		t.Errorf("Workers.MaxWorkers = %d, want %d (default preserved)", got, want)
	}
}

func TestLoadAPIEnvOverrides(t *testing.T) {
	const envSecret = "from-env-padded-to-32-byte-min!!"
	t.Setenv("OTHERIX_SERVER__LISTEN", "0.0.0.0:9999")
	t.Setenv("OTHERIX_AUTH__JWT_SECRET", envSecret)
	cfg, err := LoadAPI("testdata/api.yaml")
	if err != nil {
		t.Fatalf("LoadAPI(testdata/api.yaml) = %v, want no error", err)
	}

	if got, want := cfg.Server.Listen, "0.0.0.0:9999"; got != want {
		t.Errorf("Server.Listen = %q, want %q (env override)", got, want)
	}
	if got, want := cfg.Auth.JWTSecret, envSecret; got != want {
		t.Errorf("Auth.JWTSecret = %q, want %q (env override)", got, want)
	}
}

func TestLoadAPIRejectsBadAccessMode(t *testing.T) {
	t.Setenv("OTHERIX_CONSOLE__ACCESS_MODE", "bogus")
	_, err := LoadAPI("testdata/api.yaml")
	if err == nil {
		t.Fatalf("LoadAPI(testdata/api.yaml) = nil, want error mentioning access_mode")
	}
	if !strings.Contains(err.Error(), "access_mode") {
		t.Errorf("LoadAPI error = %q, want substring %q", err.Error(), "access_mode")
	}
}

func TestLoadAgent(t *testing.T) {
	cfg, err := LoadAgent("testdata/agent.yaml")
	if err != nil {
		t.Fatalf("LoadAgent(testdata/agent.yaml) = %v, want no error", err)
	}

	if got, want := cfg.ControlPlane.URL, "https://cp.example.local:8080"; got != want {
		t.Errorf("ControlPlane.URL = %q, want %q", got, want)
	}
	if got, want := cfg.Migration.Host, "10.0.10.5"; got != want {
		t.Errorf("Migration.Host = %q, want %q", got, want)
	}
	if got, want := cfg.Migration.PortRangeStart, 50000; got != want {
		t.Errorf("Migration.PortRangeStart = %d, want %d", got, want)
	}
	if got, want := cfg.Migration.PortRangeEnd, 50099; got != want {
		t.Errorf("Migration.PortRangeEnd = %d, want %d", got, want)
	}
	if got, want := cfg.ControlPlane.HeartbeatInterval, 30*time.Second; got != want {
		t.Errorf("ControlPlane.HeartbeatInterval = %v, want %v (default preserved)", got, want)
	}
	if got, want := cfg.WireGuard.ListenPort, 51820; got != want {
		t.Errorf("WireGuard.ListenPort = %d, want %d", got, want)
	}
	if got, want := cfg.WireGuard.PersistentKeepalive, 25*time.Second; got != want {
		t.Errorf("WireGuard.PersistentKeepalive = %v, want %v", got, want)
	}
	if got, want := cfg.WireGuard.PrivateKeyPath, "/var/lib/otherix/wg/private.key"; got != want {
		t.Errorf("WireGuard.PrivateKeyPath = %q, want %q", got, want)
	}
}

func TestLoadAgentBadPortRange(t *testing.T) {
	t.Setenv("OTHERIX_MIGRATION__PORT_RANGE_END", "100")
	_, err := LoadAgent("testdata/agent.yaml")
	if err == nil {
		t.Fatalf("LoadAgent(testdata/agent.yaml) = nil, want port-range error")
	}
	want := "port_range_end must be in [1024, 65535]"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("LoadAgent error = %q, want substring %q", err.Error(), want)
	}
}

func TestLoadAPIDefaultsOnly(t *testing.T) {
	// The defaults are deliberately incomplete: the operator must supply a
	// jwt_secret. The database dsn is no longer required on the etcd backend,
	// so the first missing-config failure is now the auth secret.
	cfg, err := LoadAPI("")
	if err == nil {
		t.Fatalf("LoadAPI(\"\") = nil, want error (no jwt_secret in defaults)")
	}
	if !strings.Contains(err.Error(), "jwt_secret") {
		t.Errorf("LoadAPI(\"\") error = %q, want substring %q", err.Error(), "jwt_secret")
	}
	_ = cfg
}

func TestLoadAgentDefaultsOnly(t *testing.T) {
	_, err := LoadAgent("")
	if err == nil {
		t.Fatalf("LoadAgent(\"\") = nil, want error (no control_plane.url in defaults)")
	}
	if !strings.Contains(err.Error(), "control_plane.url is required") {
		t.Errorf("LoadAgent(\"\") error = %q, want substring %q", err.Error(), "control_plane.url is required")
	}
}
