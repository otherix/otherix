// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cloudinit

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestInjectHostname(t *testing.T) {
	tests := []struct {
		name        string
		userData    string
		hostname    string
		wantContain []string
		wantOmit    []string
		wantErr     error
		wantNoOp    bool
	}{
		{
			name:        "empty user-data emits minimal cloud-config",
			userData:    "",
			hostname:    "vm-1",
			wantContain: []string{"#cloud-config", "hostname: vm-1"},
		},
		{
			name:        "whitespace-only user-data emits minimal cloud-config",
			userData:    "   \n\t\n",
			hostname:    "vm-2",
			wantContain: []string{"#cloud-config", "hostname: vm-2"},
		},
		{
			name:        "user-data without hostname gets injection",
			userData:    "#cloud-config\nusers:\n  - name: ubuntu\n",
			hostname:    "vm-3",
			wantContain: []string{"#cloud-config", "hostname: vm-3", "users:"},
		},
		{
			name:        "user-data with hostname is left untouched",
			userData:    "#cloud-config\nhostname: operator-pinned\nusers:\n  - name: ubuntu\n",
			hostname:    "vm-4",
			wantContain: []string{"hostname: operator-pinned"},
			wantOmit:    []string{"hostname: vm-4"},
			wantNoOp:    true,
		},
		{
			name:        "user-data without header gets injection but no header back",
			userData:    "users:\n  - name: ubuntu\n",
			hostname:    "vm-5",
			wantContain: []string{"hostname: vm-5"},
			wantOmit:    []string{"#cloud-config"},
		},
		{
			name:     "empty hostname returns ErrEmptyHostname",
			userData: "#cloud-config\n",
			hostname: "",
			wantErr:  ErrEmptyHostname,
		},
		{
			name:        "non-mapping top-level YAML left untouched",
			userData:    "#cloud-config\n- one\n- two\n",
			hostname:    "vm-6",
			wantContain: []string{"- one", "- two"},
			wantOmit:    []string{"hostname: vm-6"},
			wantNoOp:    true,
		},
		{
			name:        "user-data with hostname as nested key still gets top-level injection",
			userData:    "#cloud-config\nwrite_files:\n  - path: /etc/notes\n    content: \"some content\"\n",
			hostname:    "vm-7",
			wantContain: []string{"hostname: vm-7", "write_files"},
		},
		{
			name:        "user-data with empty mapping under header gets minimal injection",
			userData:    "#cloud-config\n",
			hostname:    "vm-8",
			wantContain: []string{"#cloud-config", "hostname: vm-8"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := injectHostname([]byte(tc.userData), tc.hostname)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("injectHostname returned err = %v", err)
			}
			if tc.wantNoOp {
				if !bytes.Equal(out, []byte(tc.userData)) {
					t.Errorf("no-op expected, got modified output:\n%s", out)
				}
			}
			for _, want := range tc.wantContain {
				if !strings.Contains(string(out), want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			for _, omit := range tc.wantOmit {
				if strings.Contains(string(out), omit) {
					t.Errorf("output unexpectedly contains %q:\n%s", omit, out)
				}
			}
		})
	}
}
