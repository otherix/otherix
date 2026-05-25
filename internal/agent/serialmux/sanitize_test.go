// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package serialmux

import (
	"bytes"
	"testing"
)

func TestSanitize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []byte
		want []byte
	}{
		{
			name: "empty",
			in:   []byte{},
			want: []byte{},
		},
		{
			name: "pure ASCII printable",
			in:   []byte("hello world"),
			want: []byte("hello world"),
		},
		{
			name: "strip NUL",
			in:   []byte{'a', 0x00, 'b'},
			want: []byte("ab"),
		},
		{
			name: "strip BEL",
			in:   []byte{'a', 0x07, 'b'},
			want: []byte("ab"),
		},
		{
			name: "preserve TAB",
			in:   []byte("a\tb"),
			want: []byte("a\tb"),
		},
		{
			name: "preserve LF",
			in:   []byte("a\nb"),
			want: []byte("a\nb"),
		},
		{
			name: "strip standalone CR",
			in:   []byte("a\rb"),
			want: []byte("ab"),
		},
		{
			name: "normalize CRLF to LF",
			in:   []byte("a\r\nb"),
			want: []byte("a\nb"),
		},
		{
			name: "strip BS VT FF other ctrl",
			in:   []byte{'a', 0x01, 0x02, 0x03, 0x08, 0x0B, 0x0C, 'b'},
			want: []byte("ab"),
		},
		{
			name: "strip DEL",
			in:   []byte{'a', 0x7F, 'b'},
			want: []byte("ab"),
		},
		{
			name: "preserve ANSI CSI red sequence",
			in:   []byte("\x1b[31mred\x1b[0m"),
			want: []byte("\x1b[31mred\x1b[0m"),
		},
		{
			name: "preserve ANSI CSI multi-parameter",
			in:   []byte("\x1b[1;33;40mboot\x1b[0m"),
			want: []byte("\x1b[1;33;40mboot\x1b[0m"),
		},
		{
			name: "preserve ANSI CSI cursor move",
			in:   []byte("\x1b[2;5H"),
			want: []byte("\x1b[2;5H"),
		},
		{
			name: "preserve UTF-8 cyrillic",
			in:   []byte("Привет"),
			want: []byte("Привет"),
		},
		{
			name: "preserve UTF-8 emoji",
			in:   []byte("ok 🚀 done"),
			want: []byte("ok 🚀 done"),
		},
		{
			name: "mixed boot output",
			in:   []byte("boot\r\nmsg\x07\x1b[32mok\x1b[0m\r\n"),
			want: []byte("boot\nmsg\x1b[32mok\x1b[0m\n"),
		},
		{
			name: "lone ESC not followed by CSI introducer dropped",
			in:   []byte("a\x1bbc"),
			want: []byte("abc"),
		},
		{
			name: "incomplete CSI at chunk tail dropped",
			in:   []byte("data\x1b[3"),
			want: []byte("data"),
		},
		{
			name: "CSI with parameter intermediate byte preserved",
			in:   []byte("\x1b[?25h"),
			want: []byte("\x1b[?25h"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sanitize(tc.in)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeDoesNotMutateInput pins that callers can reuse the input
// buffer after the call returns; the multiplexer pump reuses a 4096-
// byte scratch buffer per Read.
func TestSanitizeDoesNotMutateInput(t *testing.T) {
	t.Parallel()
	in := []byte("a\r\nb\x07c\x1b[31m!")
	cp := append([]byte(nil), in...)
	_ = sanitize(in)
	if !bytes.Equal(in, cp) {
		t.Errorf("sanitize mutated input: got %q, want %q", in, cp)
	}
}
