// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package serialmux

import (
	"bytes"
	"sync"
	"testing"
)

func TestRingBuffer_WriteAndSnapshot(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		maxBytes int
		writes   [][]byte
		wantTail []byte
	}{
		{
			name:     "empty buffer no writes",
			maxBytes: 16,
			wantTail: []byte{},
		},
		{
			name:     "single write under capacity",
			maxBytes: 16,
			writes:   [][]byte{[]byte("abc")},
			wantTail: []byte("abc"),
		},
		{
			name:     "single write equal to capacity",
			maxBytes: 4,
			writes:   [][]byte{[]byte("abcd")},
			wantTail: []byte("abcd"),
		},
		{
			name:     "second write triggers eviction",
			maxBytes: 4,
			writes:   [][]byte{[]byte("abc"), []byte("def")},
			wantTail: []byte("cdef"),
		},
		{
			name:     "single write larger than capacity keeps suffix",
			maxBytes: 4,
			writes:   [][]byte{[]byte("0123456789")},
			wantTail: []byte("6789"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := NewRingBuffer(tc.maxBytes)
			for _, w := range tc.writes {
				r.Write(w)
			}
			got := r.Snapshot()
			if !bytes.Equal(got, tc.wantTail) {
				t.Errorf("Snapshot() = %q, want %q", got, tc.wantTail)
			}
		})
	}
}

func TestRingBuffer_LastNLines(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		seed []byte
		n    int
		want []byte
	}{
		{name: "empty buffer", seed: []byte{}, n: 1, want: []byte{}},
		{name: "zero n", seed: []byte("a\nb\n"), n: 0, want: []byte{}},
		{name: "negative n", seed: []byte("a\nb\n"), n: -3, want: []byte{}},
		{name: "single line no newline", seed: []byte("abc"), n: 1, want: []byte("abc")},
		{name: "single line no newline n too high", seed: []byte("abc"), n: 5, want: []byte("abc")},
		{name: "single line trailing newline", seed: []byte("a\n"), n: 1, want: []byte("a\n")},
		{name: "two lines tail 1", seed: []byte("a\nb\n"), n: 1, want: []byte("b\n")},
		{name: "two lines tail 2", seed: []byte("a\nb\n"), n: 2, want: []byte("a\nb\n")},
		{name: "two lines tail 5", seed: []byte("a\nb\n"), n: 5, want: []byte("a\nb\n")},
		{name: "partial last line tail 1", seed: []byte("a\nb"), n: 1, want: []byte("b")},
		{name: "partial last line tail 2", seed: []byte("a\nb"), n: 2, want: []byte("a\nb")},
		{name: "three lines no trailing newline tail 2", seed: []byte("a\nb\nc"), n: 2, want: []byte("b\nc")},
		{name: "three lines trailing newline tail 2", seed: []byte("a\nb\nc\n"), n: 2, want: []byte("b\nc\n")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := NewRingBuffer(64)
			if len(tc.seed) > 0 {
				r.Write(tc.seed)
			}
			got := r.LastNLines(tc.n)
			if !bytes.Equal(got, tc.want) {
				t.Errorf("LastNLines(%d) on %q = %q, want %q", tc.n, tc.seed, got, tc.want)
			}
		})
	}
}

// TestRingBuffer_SnapshotIsCopy pins that the returned slice is
// independent of the buffer's internal storage - callers may hold it
// indefinitely without blocking subsequent Writes.
func TestRingBuffer_SnapshotIsCopy(t *testing.T) {
	t.Parallel()
	r := NewRingBuffer(8)
	r.Write([]byte("hello"))
	snap := r.Snapshot()
	r.Write([]byte("XXX"))
	if !bytes.Equal(snap, []byte("hello")) {
		t.Errorf("Snapshot aliased buffer storage: got %q, want %q", snap, "hello")
	}
}

// TestRingBuffer_ConcurrentWritesAndReads exercises the mutex contract
// - race detector would flag any unsynchronised access here.
func TestRingBuffer_ConcurrentWritesAndReads(t *testing.T) {
	t.Parallel()
	r := NewRingBuffer(256)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 200 {
			r.Write([]byte("line\n"))
		}
	}()
	go func() {
		defer wg.Done()
		for range 200 {
			_ = r.LastNLines(5)
		}
	}()
	wg.Wait()
}
