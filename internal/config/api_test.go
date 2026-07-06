// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

func TestResourcesConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ResourcesConfig
		wantErr string
	}{
		{name: "production defaults", cfg: defaultResourcesConfig()},
		{
			name: "all disabled, ratios still positive",
			cfg: ResourcesConfig{
				CPU:    ResourceConfig{Enabled: false, OvercommitRatio: 1.0},
				Memory: ResourceConfig{Enabled: false, OvercommitRatio: 1.0},
				Disk:   ResourceConfig{Enabled: false, OvercommitRatio: 1.0},
			},
		},
		{
			name: "mixed overcommit ratios",
			cfg: ResourcesConfig{
				CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 2.0},
				Memory: ResourceConfig{Enabled: true, OvercommitRatio: 0.8},
				Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
			},
		},
		{
			name: "cpu zero ratio (disabled)",
			cfg: ResourcesConfig{
				CPU:    ResourceConfig{Enabled: false, OvercommitRatio: 0.0},
				Memory: ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
			},
			wantErr: "placement.resources.cpu.overcommit_ratio must be > 0",
		},
		{
			name: "memory negative ratio",
			cfg: ResourcesConfig{
				CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				Memory: ResourceConfig{Enabled: true, OvercommitRatio: -1.0},
				Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
			},
			wantErr: "placement.resources.memory.overcommit_ratio must be > 0",
		},
		{
			name: "disk zero ratio (enabled)",
			cfg: ResourcesConfig{
				CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				Memory: ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 0.0},
			},
			wantErr: "placement.resources.disk.overcommit_ratio must be > 0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestPressureConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     PressureConfig
		wantErr string
	}{
		{name: "defaults pass", cfg: defaultPressureConfig()},
		{
			name: "threshold below range",
			cfg: PressureConfig{Memory: PressureConditionConfig{
				Enabled: true, ThresholdPercent: 0, ConsecutiveRequired: 3,
			}},
			wantErr: "placement.pressure.memory.threshold_percent must be in [1, 99]",
		},
		{
			name: "threshold above range",
			cfg: PressureConfig{Memory: PressureConditionConfig{
				Enabled: true, ThresholdPercent: 100, ConsecutiveRequired: 3,
			}},
			wantErr: "placement.pressure.memory.threshold_percent must be in [1, 99]",
		},
		{
			name: "consecutive zero",
			cfg: PressureConfig{Memory: PressureConditionConfig{
				Enabled: true, ThresholdPercent: 10, ConsecutiveRequired: 0,
			}},
			wantErr: "placement.pressure.memory.consecutive_required must be >= 1",
		},
		{
			name: "enabled false still validates ranges (config hygiene)",
			cfg: PressureConfig{Memory: PressureConditionConfig{
				Enabled: false, ThresholdPercent: 200, ConsecutiveRequired: 3,
			}},
			wantErr: "placement.pressure.memory.threshold_percent must be in [1, 99]",
		},
		{
			name: "system_disk threshold below range",
			cfg: PressureConfig{
				Memory:     defaultPressureConfig().Memory,
				SystemDisk: PressureConditionConfig{Enabled: true, ThresholdPercent: 0, ConsecutiveRequired: 3},
				Disk:       defaultPressureConfig().Disk,
			},
			wantErr: "placement.pressure.system_disk.threshold_percent must be in [1, 99]",
		},
		{
			name: "disk consecutive zero",
			cfg: PressureConfig{
				Memory:     defaultPressureConfig().Memory,
				SystemDisk: defaultPressureConfig().SystemDisk,
				Disk:       PressureConditionConfig{Enabled: true, ThresholdPercent: 15, ConsecutiveRequired: 0},
			},
			wantErr: "placement.pressure.disk.consecutive_required must be >= 1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestPlacementConfig_Validate_ResourcesPropagated(t *testing.T) {
	// PlacementConfig.Validate must surface ResourcesConfig errors so
	// the api-server fails fast at startup rather than on the hot path.
	cfg := PlacementConfig{
		Algorithm: "resource_aware",
		Resources: ResourcesConfig{
			CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
			Memory: ResourceConfig{Enabled: true, OvercommitRatio: -1.0},
			Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "memory.overcommit_ratio") {
		t.Errorf("Validate() = %v, want memory.overcommit_ratio error", err)
	}
}

func TestPlacementConfig_Warnings(t *testing.T) {
	// Each case asserts the operator-facing warning content surfaced
	// by `Warnings()`. Substrings are checked rather than full strings
	// so the format helper can evolve without touching every case.
	tests := []struct {
		name     string
		cfg      PlacementConfig
		wantLen  int
		contains []string
	}{
		{
			name:    "defaults — no warnings",
			cfg:     PlacementConfig{Algorithm: "resource_aware", Resources: defaultResourcesConfig()},
			wantLen: 0,
		},
		{
			name: "cpu overcommit 1.5 — neutral language",
			cfg: PlacementConfig{
				Algorithm: "resource_aware",
				Resources: ResourcesConfig{
					CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.5},
					Memory: ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
					Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				},
			},
			wantLen:  1,
			contains: []string{"cpu.overcommit_ratio=1.50", "overcommit enabled"},
		},
		{
			name: "memory overcommit 1.2 — OOM risk language",
			cfg: PlacementConfig{
				Algorithm: "resource_aware",
				Resources: ResourcesConfig{
					CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
					Memory: ResourceConfig{Enabled: true, OvercommitRatio: 1.2},
					Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				},
			},
			wantLen:  1,
			contains: []string{"memory.overcommit_ratio=1.20", "OOM kill risk"},
		},
		{
			name: "memory overcommit 2.5 — extreme bucket",
			cfg: PlacementConfig{
				Algorithm: "resource_aware",
				Resources: ResourcesConfig{
					CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
					Memory: ResourceConfig{Enabled: true, OvercommitRatio: 2.5},
					Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				},
			},
			wantLen:  1,
			contains: []string{"memory.overcommit_ratio=2.50", "extreme overcommit", "OOM kill risk"},
		},
		{
			name: "disk overcommit 1.3 — sparse qcow2 risk",
			cfg: PlacementConfig{
				Algorithm: "resource_aware",
				Resources: ResourcesConfig{
					CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
					Memory: ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
					Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.3},
				},
			},
			wantLen:  1,
			contains: []string{"disk.overcommit_ratio=1.30", "sparse qcow2"},
		},
		{
			name: "memory overcommit ignored when disabled",
			cfg: PlacementConfig{
				Algorithm: "resource_aware",
				Resources: ResourcesConfig{
					CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
					Memory: ResourceConfig{Enabled: false, OvercommitRatio: 3.0},
					Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				},
			},
			wantLen: 0,
		},
		{
			name: "all three disabled — count fallback warning",
			cfg: PlacementConfig{
				Algorithm: "resource_aware",
				Resources: ResourcesConfig{
					CPU:    ResourceConfig{Enabled: false, OvercommitRatio: 1.0},
					Memory: ResourceConfig{Enabled: false, OvercommitRatio: 1.0},
					Disk:   ResourceConfig{Enabled: false, OvercommitRatio: 1.0},
				},
			},
			wantLen:  1,
			contains: []string{"all dimensions disabled", "count-based"},
		},
		{
			name: "mixed: cpu overcommit + memory extreme + all-disabled not triggered",
			cfg: PlacementConfig{
				Algorithm: "resource_aware",
				Resources: ResourcesConfig{
					CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.5},
					Memory: ResourceConfig{Enabled: true, OvercommitRatio: 2.5},
					Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
				},
			},
			wantLen:  2,
			contains: []string{"cpu.overcommit_ratio=1.50", "memory.overcommit_ratio=2.50", "extreme overcommit"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.Warnings()
			if len(got) != tc.wantLen {
				t.Fatalf("Warnings() returned %d entries, want %d\nentries=%q", len(got), tc.wantLen, got)
			}
			joined := strings.Join(got, "\n")
			for _, sub := range tc.contains {
				if !strings.Contains(joined, sub) {
					t.Errorf("Warnings() missing substring %q in %q", sub, joined)
				}
			}
		})
	}
}

func TestStoragePoolScanConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     StoragePoolScanConfig
		wantErr string
	}{
		{
			name: "production defaults",
			cfg:  StoragePoolScanConfig{Enabled: true, Interval: 15 * time.Minute, Jitter: 30 * time.Second},
		},
		{
			name: "zero values fall through to worker defaults",
			cfg:  StoragePoolScanConfig{},
		},
		{
			name: "disabled with non-zero timings",
			cfg:  StoragePoolScanConfig{Enabled: false, Interval: time.Hour, Jitter: time.Second},
		},
		{
			name:    "negative interval",
			cfg:     StoragePoolScanConfig{Interval: -time.Second},
			wantErr: "interval must be >= 0",
		},
		{
			name:    "negative jitter",
			cfg:     StoragePoolScanConfig{Interval: time.Minute, Jitter: -time.Second},
			wantErr: "jitter must be >= 0",
		},
		{
			name:    "jitter equal interval",
			cfg:     StoragePoolScanConfig{Interval: time.Minute, Jitter: time.Minute},
			wantErr: "must be less than interval",
		},
		{
			name:    "jitter exceeds interval",
			cfg:     StoragePoolScanConfig{Interval: time.Minute, Jitter: 2 * time.Minute},
			wantErr: "must be less than interval",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestBackupConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     BackupConfig
		wantErr bool
	}{
		{name: "disabled, no dir", cfg: BackupConfig{Enabled: false}, wantErr: false},
		{name: "enabled with dir", cfg: BackupConfig{Enabled: true, Dir: "/var/backups", Retention: 7}, wantErr: false},
		{name: "enabled, no dir", cfg: BackupConfig{Enabled: true}, wantErr: true},
		{name: "negative retention", cfg: BackupConfig{Enabled: true, Dir: "/d", Retention: -1}, wantErr: true},
		{name: "negative interval", cfg: BackupConfig{Enabled: true, Dir: "/d", Interval: -1}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if gotErr := tc.cfg.Validate() != nil; gotErr != tc.wantErr {
				t.Errorf("Validate() err present = %v, want %v", gotErr, tc.wantErr)
			}
		})
	}
}

func TestNetworkConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     NetworkConfig
		wantErr bool
	}{
		{name: "underlay_mtu 0 (default)", cfg: NetworkConfig{UnderlayMTU: 0, VNIRange: VNIRangeConfig{Min: 1000, Max: 65535}}, wantErr: false},
		{name: "underlay_mtu 128 (too low)", cfg: NetworkConfig{UnderlayMTU: 128, VNIRange: VNIRangeConfig{Min: 1000, Max: 65535}}, wantErr: true},
		{name: "underlay_mtu 1389 (just below floor)", cfg: NetworkConfig{UnderlayMTU: 1389, VNIRange: VNIRangeConfig{Min: 1000, Max: 65535}}, wantErr: true},
		{name: "underlay_mtu 1390 (floor)", cfg: NetworkConfig{UnderlayMTU: 1390, VNIRange: VNIRangeConfig{Min: 1000, Max: 65535}}, wantErr: false},
		{name: "underlay_mtu 65536 (too high)", cfg: NetworkConfig{UnderlayMTU: 65536, VNIRange: VNIRangeConfig{Min: 1000, Max: 65535}}, wantErr: true},
		{name: "vni_range 0/0 (default)", cfg: NetworkConfig{UnderlayMTU: 1500, VNIRange: VNIRangeConfig{Min: 0, Max: 0}}, wantErr: false},
		{name: "vni_range min >= max", cfg: NetworkConfig{UnderlayMTU: 1500, VNIRange: VNIRangeConfig{Min: 2000, Max: 2000}}, wantErr: true},
		{name: "vni_range min < 1000", cfg: NetworkConfig{UnderlayMTU: 1500, VNIRange: VNIRangeConfig{Min: 999, Max: 2000}}, wantErr: true},
		{name: "vni_range max > 16777215", cfg: NetworkConfig{UnderlayMTU: 1500, VNIRange: VNIRangeConfig{Min: 1000, Max: 16777216}}, wantErr: true},
		{name: "vni_range valid widened", cfg: NetworkConfig{UnderlayMTU: 1500, VNIRange: VNIRangeConfig{Min: 1000, Max: 16777215}}, wantErr: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if gotErr := tc.cfg.Validate() != nil; gotErr != tc.wantErr {
				t.Errorf("Validate() err present = %v, want %v", gotErr, tc.wantErr)
			}
		})
	}
}

func TestDefaultAPIConfig_VNIRange(t *testing.T) {
	cfg := defaultAPIConfig()
	if cfg.Network.VNIRange.Min != 1000 {
		t.Errorf("default VNIRange.Min = %d, want 1000", cfg.Network.VNIRange.Min)
	}
	if cfg.Network.VNIRange.Max != 65535 {
		t.Errorf("default VNIRange.Max = %d, want 65535", cfg.Network.VNIRange.Max)
	}
}

func TestDefaultAPIConfig_EtcdSingleNode(t *testing.T) {
	got := defaultAPIConfig().Etcd
	want := EtcdConfig{
		Mode:         "single",
		Name:         "otherix-0",
		DataDir:      "/var/lib/otherix/etcd",
		PeerURL:      "auto",
		ClientURL:    "http://127.0.0.1:2379",
		ClusterToken: "otherix-cluster",
		PeerAutoDir:  "/var/lib/otherix/peer",
	}
	if got != want {
		t.Errorf("defaultAPIConfig().Etcd = %+v, want %+v", got, want)
	}
}

func TestDefaultAPIConfig_StoragePoolsDefaultPoolName(t *testing.T) {
	got := defaultAPIConfig().StoragePools.DefaultPoolName
	if got != "default" {
		t.Errorf("defaultAPIConfig().StoragePools.DefaultPoolName = %q, want %q", got, "default")
	}
}

func TestStoragePoolsConfig_ValidateDefaultPoolName(t *testing.T) {
	tests := []struct {
		name    string
		pool    string
		wantErr bool
	}{
		{name: "valid name", pool: "default"},
		{name: "valid hyphenated name", pool: "team-pool-1"},
		{name: "empty opts out", pool: ""},
		{name: "leading whitespace rejected", pool: " bad", wantErr: true},
		{name: "trailing whitespace rejected", pool: "bad ", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := StoragePoolsConfig{
				AllowedPathPrefixes: []string{"/var/lib/otherix/pools/"},
				DefaultPoolName:     tc.pool,
			}
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() with DefaultPoolName=%q = nil, want error", tc.pool)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() with DefaultPoolName=%q = %v, want nil", tc.pool, err)
			}
		})
	}
}

// repoRoot walks up from the test working directory until it finds go.mod,
// so the test can resolve repo-relative config paths regardless of how the
// test binary is invoked.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() = %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found walking up from test working directory")
		}
		dir = parent
	}
}

// jwtSecretFromYAML extracts auth.jwt_secret from the YAML config file at
// path. It fails the test if the file is unreadable or the key is absent,
// so a renamed key surfaces as a test failure rather than a silent pass.
func jwtSecretFromYAML(t *testing.T, path string) string {
	t.Helper()
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		t.Fatalf("load %q: %v", path, err)
	}
	secret := k.String("auth.jwt_secret")
	if secret == "" {
		t.Fatalf("%q has no auth.jwt_secret value", path)
	}
	return secret
}

// TestAuthConfig_Validate_ShippedConfigSeam guards the seam between the
// shipped config files and jwtSecretDenylist: the public example placeholder
// MUST fail startup (a verbatim copy of the example file is a publicly known
// signing key), while the dev-stack config MUST keep booting because
// local-dev-start runs the api against it. Changing either file's jwt_secret
// without updating the denylist fails here.
func TestAuthConfig_Validate_ShippedConfigSeam(t *testing.T) {
	root := repoRoot(t)
	validTTLs := AuthConfig{
		JWTAccessTTL:  15 * time.Minute,
		JWTRefreshTTL: 720 * time.Hour,
	}

	t.Run("example config secret must be rejected", func(t *testing.T) {
		cfg := validTTLs
		cfg.JWTSecret = jwtSecretFromYAML(t, filepath.Join(root, "deploy", "config", "api.example.yaml"))
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate() with example-file jwt_secret = nil, want error (shipped example must fail startup)")
		}
	})

	t.Run("dev stack config secret must pass", func(t *testing.T) {
		cfg := validTTLs
		cfg.JWTSecret = jwtSecretFromYAML(t, filepath.Join(root, "dev", "config", "api.yaml"))
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() with dev-stack jwt_secret = %v, want nil (local dev stack must boot)", err)
		}
	})
}

func TestAuthConfig_Validate_JWTSecret(t *testing.T) {
	tests := []struct {
		name    string
		secret  string
		wantErr bool
	}{
		{
			name:   "valid random secret passes",
			secret: "k9Qz3vR7mX1pL5wT8nB2cJ6hF0dY4sGa",
		},
		{
			name:    "too short rejected",
			secret:  "short-secret",
			wantErr: true,
		},
		{
			name:    "old shipped example value denylisted",
			secret:  "dev-only-jwt-secret-32-bytes-pad!",
			wantErr: true,
		},
		{
			name:    "explicit placeholder denylisted",
			secret:  "CHANGE_ME_set_a_unique_random_32plus_byte_secret",
			wantErr: true,
		},
		{
			name:    "single repeated byte rejected",
			secret:  strings.Repeat("x", 40),
			wantErr: true,
		},
		{
			name:    "docs placeholder denylisted",
			secret:  "REPLACE-ME-with-openssl-rand-hex-32-output",
			wantErr: true,
		},
		{
			name:    "denylisted value with surrounding whitespace rejected",
			secret:  "  dev-only-jwt-secret-32-bytes-pad!  ",
			wantErr: true,
		},
		{
			name:    "all-whitespace secret rejected",
			secret:  strings.Repeat(" ", 40),
			wantErr: true,
		},
		{
			name:    "whitespace padding does not satisfy minimum length",
			secret:  strings.Repeat("a1b2c3d", 4) + "xyz ", // 31 real bytes + trailing space
			wantErr: true,
		},
		{
			name:   "dev stack secret passes",
			secret: "otherix-local-dev-stack-signing-key-32b",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := AuthConfig{
				JWTSecret:     tc.secret,
				JWTAccessTTL:  15 * time.Minute,
				JWTRefreshTTL: 720 * time.Hour,
			}
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() with JWTSecret=%q = nil, want error", tc.secret)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() with JWTSecret=%q = %v, want nil", tc.secret, err)
			}
		})
	}
}

func TestHeartbeatWorkersConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     HeartbeatWorkersConfig
		wantErr bool
	}{
		{"grace exceeds threshold", HeartbeatWorkersConfig{StaleThreshold: 90 * time.Second, RebalanceGrace: 5 * time.Minute, Interval: 30 * time.Second}, false},
		{"grace equals threshold", HeartbeatWorkersConfig{StaleThreshold: 5 * time.Minute, RebalanceGrace: 5 * time.Minute, Interval: 30 * time.Second}, false},
		{"grace below threshold", HeartbeatWorkersConfig{StaleThreshold: 5 * time.Minute, RebalanceGrace: 90 * time.Second, Interval: 30 * time.Second}, true},
	}
	for _, tt := range tests {
		err := tt.cfg.Validate()
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: Validate() error = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
	}
}
