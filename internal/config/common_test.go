// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"strings"
	"testing"
)

func TestMigrationConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     MigrationConfig
		wantErr string // empty string means no error expected
	}{
		{
			name: "valid range",
			cfg:  MigrationConfig{Host: "10.0.0.1", PortRangeStart: 49152, PortRangeEnd: 49251},
		},
		{
			name: "single port range",
			cfg:  MigrationConfig{Host: "10.0.0.1", PortRangeStart: 49200, PortRangeEnd: 49200},
		},
		{
			name:    "end before start",
			cfg:     MigrationConfig{Host: "10.0.0.1", PortRangeStart: 49200, PortRangeEnd: 49100},
			wantErr: "port_range_end must be >= port_range_start",
		},
		{
			name:    "start below 1024",
			cfg:     MigrationConfig{Host: "10.0.0.1", PortRangeStart: 80, PortRangeEnd: 81},
			wantErr: "port_range_start must be in [1024, 65535]",
		},
		{
			name:    "end above 65535",
			cfg:     MigrationConfig{Host: "10.0.0.1", PortRangeStart: 49152, PortRangeEnd: 70000},
			wantErr: "port_range_end must be in [1024, 65535]",
		},
		{
			name:    "missing endpoint host",
			cfg:     MigrationConfig{PortRangeStart: 49152, PortRangeEnd: 49251},
			wantErr: "host is required",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("MigrationConfig.Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("MigrationConfig.Validate() = nil, want error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("MigrationConfig.Validate() = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}
