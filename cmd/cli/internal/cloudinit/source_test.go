// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cloudinit

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSource(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "cloud.yaml")
	want := []byte("#cloud-config\nusers:\n  - name: ubuntu\n")
	if err := os.WriteFile(yamlPath, want, 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	tests := []struct {
		name       string
		path       string
		stdin      io.Reader
		want       []byte
		wantErrIs  error
		wantErrSub string
	}{
		{
			name:      "empty path returns ErrEmptyPath",
			path:      "",
			wantErrIs: ErrEmptyPath,
		},
		{
			name: "file path returns full content",
			path: yamlPath,
			want: want,
		},
		{
			name:  "stdin sentinel reads io.Reader",
			path:  StdinSentinel,
			stdin: bytes.NewBufferString("#cloud-config\npackages: [htop]\n"),
			want:  []byte("#cloud-config\npackages: [htop]\n"),
		},
		{
			name:       "missing file surfaces underlying error",
			path:       filepath.Join(dir, "does-not-exist.yaml"),
			wantErrSub: "does-not-exist.yaml",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.stdin != nil {
				prev := stdinReader
				stdinReader = tc.stdin
				defer func() { stdinReader = prev }()
			}
			got, err := ReadSource(tc.path)
			if tc.wantErrIs != nil {
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("err = %v, want %v", err, tc.wantErrIs)
				}
				return
			}
			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadSource err = %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("ReadSource = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantWarnSub  string
		wantErrPhase string
	}{
		{
			name: "valid cloud-config",
			body: "#cloud-config\nusers:\n  - name: ubuntu\n",
		},
		{
			name:        "empty body emits warning, no error",
			body:        "",
			wantWarnSub: "empty",
		},
		{
			name:        "whitespace-only body emits warning",
			body:        "   \n\t\n",
			wantWarnSub: "empty",
		},
		{
			name:        "missing directive emits warning",
			body:        "users:\n  - name: ubuntu\n",
			wantWarnSub: "#cloud-config",
		},
		{
			name:         "malformed YAML returns Diagnostic",
			body:         "#cloud-config\nusers: [unterminated",
			wantErrPhase: "yaml_parse",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			warnings, err := Validate([]byte(tc.body))
			if tc.wantErrPhase != "" {
				var diag *Diagnostic
				if !errors.As(err, &diag) {
					t.Fatalf("err = %v, want *Diagnostic", err)
				}
				if diag.Phase != tc.wantErrPhase {
					t.Errorf("Phase = %q, want %q", diag.Phase, tc.wantErrPhase)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate err = %v", err)
			}
			if tc.wantWarnSub == "" {
				if len(warnings) != 0 {
					t.Errorf("unexpected warnings: %v", warnings)
				}
				return
			}
			joined := strings.Join(warnings, " ")
			if !strings.Contains(joined, tc.wantWarnSub) {
				t.Errorf("warnings = %v, want substring %q", warnings, tc.wantWarnSub)
			}
		})
	}
}
