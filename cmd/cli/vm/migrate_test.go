// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// TestParseBandwidth covers the byte-rate parser: plain integers are bytes,
// k/m/g (and the IEC ki/mi/gi) suffixes scale, an empty string yields nil
// (server default), and garbage is an error.
func TestParseBandwidth(t *testing.T) {
	cases := []struct {
		in      string
		want    *int64
		wantErr bool
	}{
		{"", nil, false},
		{"0", ptrI64(0), false},
		{"1024", ptrI64(1024), false},
		{"100k", ptrI64(100_000), false},
		{"100K", ptrI64(100_000), false},
		{"10m", ptrI64(10_000_000), false},
		{"1g", ptrI64(1_000_000_000), false},
		{"1ki", ptrI64(1024), false},
		{"1mi", ptrI64(1024 * 1024), false},
		{"1gi", ptrI64(1024 * 1024 * 1024), false},
		{"-5", nil, true},
		{"abc", nil, true},
		{"10x", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseBandwidth(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseBandwidth(%q) err = nil, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBandwidth(%q) err = %v", tc.in, err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("parseBandwidth(%q) mismatch (-want +got):\n%s", tc.in, diff)
			}
		})
	}
}

// TestBuildMigrateBody covers the flag->body mapping: --offline flips live to
// false (default true), bandwidth parses to bytes, and unset knobs stay nil so
// the server applies its defaults.
func TestBuildMigrateBody(t *testing.T) {
	t.Run("defaults: live true, knobs unset", func(t *testing.T) {
		got, err := buildMigrateBody(migrateFlags{})
		if err != nil {
			t.Fatalf("buildMigrateBody: %v", err)
		}
		want := cpclient.MigrateBody{Live: true}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("body mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("offline flips live false", func(t *testing.T) {
		got, err := buildMigrateBody(migrateFlags{offline: true})
		if err != nil {
			t.Fatalf("buildMigrateBody: %v", err)
		}
		if got.Live {
			t.Errorf("Live = true, want false under --offline")
		}
	})

	t.Run("node/pool/knobs populate", func(t *testing.T) {
		got, err := buildMigrateBody(migrateFlags{
			node:          "node-b",
			pool:          "fast",
			bandwidth:     "100m",
			maxDowntimeMs: 250,
			allowPostcopy: true,
		})
		if err != nil {
			t.Fatalf("buildMigrateBody: %v", err)
		}
		if got.TargetNode == nil || *got.TargetNode != "node-b" {
			t.Errorf("TargetNode = %v, want node-b", got.TargetNode)
		}
		if got.TargetPool == nil || *got.TargetPool != "fast" {
			t.Errorf("TargetPool = %v, want fast", got.TargetPool)
		}
		if got.MaxBandwidthBytes == nil || *got.MaxBandwidthBytes != 100_000_000 {
			t.Errorf("MaxBandwidthBytes = %v, want 100000000", got.MaxBandwidthBytes)
		}
		if got.MaxDowntimeMs == nil || *got.MaxDowntimeMs != 250 {
			t.Errorf("MaxDowntimeMs = %v, want 250", got.MaxDowntimeMs)
		}
		if !got.AllowPostcopy {
			t.Errorf("AllowPostcopy = false, want true")
		}
	})
}

func ptrI64(v int64) *int64 { return &v }
