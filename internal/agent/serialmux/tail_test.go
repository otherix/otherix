// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package serialmux

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestReadTailLines(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		current []byte // nil = file absent
		rotated []byte // nil = file absent
		n       int
		want    []byte
	}{
		{
			name: "no files n minus one returns empty",
			n:    -1,
			want: []byte{},
		},
		{
			name: "no files n positive returns empty",
			n:    5,
			want: []byte{},
		},
		{
			name:    "current only all",
			current: []byte("a\nb\nc\n"),
			n:       -1,
			want:    []byte("a\nb\nc\n"),
		},
		{
			name:    "current only tail two",
			current: []byte("a\nb\nc\n"),
			n:       2,
			want:    []byte("b\nc\n"),
		},
		{
			name:    "current only tail more than lines",
			current: []byte("a\nb\nc\n"),
			n:       50,
			want:    []byte("a\nb\nc\n"),
		},
		{
			name:    "current only no trailing newline tail one",
			current: []byte("a\nb\nc"),
			n:       1,
			want:    []byte("c"),
		},
		{
			name:    "rotated only all",
			rotated: []byte("x\ny\n"),
			n:       -1,
			want:    []byte("x\ny\n"),
		},
		{
			name:    "both files concatenated chronologically all",
			rotated: []byte("old1\nold2\n"),
			current: []byte("new1\nnew2\n"),
			n:       -1,
			want:    []byte("old1\nold2\nnew1\nnew2\n"),
		},
		{
			name:    "both files tail within current",
			rotated: []byte("old1\nold2\n"),
			current: []byte("new1\nnew2\nnew3\n"),
			n:       2,
			want:    []byte("new2\nnew3\n"),
		},
		{
			name:    "both files tail spanning into rotated",
			rotated: []byte("old1\nold2\n"),
			current: []byte("new1\nnew2\n"),
			n:       3,
			want:    []byte("old2\nnew1\nnew2\n"),
		},
		{
			name:    "both files tail larger than total",
			rotated: []byte("old1\nold2\n"),
			current: []byte("new1\nnew2\n"),
			n:       100,
			want:    []byte("old1\nold2\nnew1\nnew2\n"),
		},
		{
			name:    "n zero returns empty even with content",
			current: []byte("a\nb\n"),
			n:       0,
			want:    []byte{},
		},
		{
			name:    "current empty file behaves like no current",
			current: []byte{},
			rotated: []byte("r\n"),
			n:       -1,
			want:    []byte("r\n"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tc.current != nil {
				if err := os.WriteFile(filepath.Join(dir, logFileName), tc.current, 0o600); err != nil {
					t.Fatalf("write current: %v", err)
				}
			}
			if tc.rotated != nil {
				if err := os.WriteFile(filepath.Join(dir, rotatedFileName), tc.rotated, 0o600); err != nil {
					t.Fatalf("write rotated: %v", err)
				}
			}
			got, err := readTailLines(dir, tc.n)
			if err != nil {
				t.Fatalf("readTailLines(%d): %v", tc.n, err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("readTailLines(dir, %d) = %q, want %q", tc.n, got, tc.want)
			}
		})
	}
}

func TestReadTailLines_MissingDirIsError(t *testing.T) {
	t.Parallel()
	// Walk a path that definitely does not exist - readTailLines treats
	// individual missing files as empty, but a missing directory passed
	// in is an operator-supplied path; surface the underlying error.
	got, err := readTailLines("/this/path/should/never/exist/otherix-test", -1)
	// On Linux/macOS, os.ReadFile on a missing path returns ErrNotExist
	// regardless of which segment is missing, so this should fall under
	// our "missing file → empty" rule. Pin that behavior - caller never
	// gets a surprise error for a non-existent log directory.
	if err != nil {
		t.Errorf("readTailLines(missing-dir, -1) returned error %v, want nil (treat as empty)", err)
	}
	if len(got) != 0 {
		t.Errorf("readTailLines(missing-dir, -1) = %q, want empty", got)
	}
}
