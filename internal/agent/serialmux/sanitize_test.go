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

// TestSanitizer_CrossChunkCSIPreserved pins the fix for the L14
// "preserve ANSI CSI verbatim" rule when a single CSI sequence is
// split across two QEMU socket reads. Without the Sanitizer carry,
// the ESC byte (or the partial parameter / intermediate bytes
// between ESC and the final byte) would be dropped at the end of
// chunk N and the trailing fragment would appear as literal text in
// chunk N+1.
//
// Real-agent Lima smoke caught 284 broken CSI fragments
// (`[0;32m`, `[0m`, etc. as literal text) against 145 intact ones
// on a single Ubuntu systemd boot before this fix was wired.
func TestSanitizer_CrossChunkCSIPreserved(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		chunks [][]byte
		want   []byte
	}{
		{
			name:   "ESC at chunk boundary",
			chunks: [][]byte{[]byte("hello\x1b"), []byte("[31mred\x1b[0m bye")},
			want:   []byte("hello\x1b[31mred\x1b[0m bye"),
		},
		{
			name:   "CSI introducer at chunk boundary",
			chunks: [][]byte{[]byte("a\x1b["), []byte("32mgreen\x1b[0m")},
			want:   []byte("a\x1b[32mgreen\x1b[0m"),
		},
		{
			name:   "CSI parameter at chunk boundary",
			chunks: [][]byte{[]byte("a\x1b[31"), []byte(";1mwarn\x1b[0m")},
			want:   []byte("a\x1b[31;1mwarn\x1b[0m"),
		},
		{
			name: "real systemd OK marker split mid-CSI",
			chunks: [][]byte{
				[]byte("[\x1b[0;3"),
				[]byte("2m  OK  \x1b[0m] Started foo.service\n"),
			},
			want: []byte("[\x1b[0;32m  OK  \x1b[0m] Started foo.service\n"),
		},
		{
			name: "three chunks, CSI split across all three",
			chunks: [][]byte{
				[]byte("first\x1b"),
				[]byte("[1;33"),
				[]byte(";40mboot\x1b[0m"),
			},
			want: []byte("first\x1b[1;33;40mboot\x1b[0m"),
		},
		{
			name: "interleaved complete and split sequences",
			chunks: [][]byte{
				[]byte("\x1b[31ma\x1b"),
				[]byte("[0mb"),
			},
			want: []byte("\x1b[31ma\x1b[0mb"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := NewSanitizer()
			var got []byte
			for _, c := range tc.chunks {
				got = append(got, s.Process(c)...)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("chunks=%q\n  got = %q\n  want = %q", tc.chunks, got, tc.want)
			}
		})
	}
}

// TestSanitizer_CarryOverflowDrops pins that an orphan ESC stream
// past maxPendingCarry is discarded, so a buggy guest cannot grow the
// per-VM Sanitizer state without bound.
func TestSanitizer_CarryOverflowDrops(t *testing.T) {
	t.Parallel()
	s := NewSanitizer()
	// Build a chunk that ends with maxPendingCarry+1 of partial CSI
	// (ESC '[' followed by enough parameter bytes to overshoot the cap
	// without ever providing a final byte).
	chunk := append([]byte("ok"), 0x1B, '[')
	for range maxPendingCarry + 4 {
		chunk = append(chunk, '0')
	}
	got1 := s.Process(chunk)
	if !bytes.Equal(got1, []byte("ok")) {
		t.Errorf("first chunk got %q, want %q", got1, "ok")
	}
	// Next chunk arrives - the dropped carry must NOT leak into it.
	got2 := s.Process([]byte("next"))
	if !bytes.Equal(got2, []byte("next")) {
		t.Errorf("second chunk got %q, want %q", got2, "next")
	}
}

// TestSanitizer_PassThroughWhenNoCarry pins that Process matches the
// stateless sanitize() result when every CSI is complete inside its
// chunk.
func TestSanitizer_PassThroughWhenNoCarry(t *testing.T) {
	t.Parallel()
	input := []byte("hello\r\nworld\n\x1b[31mred\x1b[0m end")
	want := sanitize(input)
	s := NewSanitizer()
	got := s.Process(input)
	if !bytes.Equal(got, want) {
		t.Errorf("Process(%q) = %q, want %q", input, got, want)
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
