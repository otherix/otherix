// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vms

import (
	"bytes"
	"testing"
)

func TestConsoleInboundBufferFIFOAndBytes(t *testing.T) {
	t.Parallel()
	b := newConsoleInboundBuffer(1024)
	if !b.append([]byte("ab")) {
		t.Fatalf("append(ab) = false, want true")
	}
	if !b.append([]byte("cd")) {
		t.Fatalf("append(cd) = false, want true")
	}
	got1, ok := b.take()
	if !ok || string(got1) != "ab" {
		t.Errorf("take() = %q,%v, want ab,true", got1, ok)
	}
	got2, ok := b.take()
	if !ok || string(got2) != "cd" {
		t.Errorf("take() = %q,%v, want cd,true", got2, ok)
	}
	if _, ok := b.take(); ok {
		t.Errorf("take() on empty = ok=true, want false")
	}
}

func TestConsoleInboundBufferUnshiftIsHead(t *testing.T) {
	t.Parallel()
	b := newConsoleInboundBuffer(1024)
	b.append([]byte("second"))
	b.unshift([]byte("first"))
	got, _ := b.take()
	if string(got) != "first" {
		t.Errorf("take() after unshift = %q, want first", got)
	}
}

func TestConsoleInboundBufferOverflow(t *testing.T) {
	t.Parallel()
	b := newConsoleInboundBuffer(4)
	if !b.append([]byte("abcd")) {
		t.Fatalf("append at limit = false, want true")
	}
	if b.overflowed() {
		t.Errorf("overflowed() at limit = true, want false")
	}
	if b.append([]byte("e")) {
		t.Errorf("append past limit = true, want false")
	}
	if !b.overflowed() {
		t.Errorf("overflowed() past limit = false, want true")
	}
	got, _ := b.take()
	if string(got) != "abcd" {
		t.Errorf("take() after overflow = %q, want abcd", got)
	}
	if _, ok := b.take(); ok {
		t.Errorf("overflowing frame was stored, want dropped")
	}
}

func TestConsoleInboundBufferAppendCopies(t *testing.T) {
	t.Parallel()
	b := newConsoleInboundBuffer(1024)
	src := []byte("xy")
	b.append(src)
	src[0] = 'Z'
	got, _ := b.take()
	if !bytes.Equal(got, []byte("xy")) {
		t.Errorf("take() = %q, want xy (append must copy)", got)
	}
}

func TestConsoleInboundBufferSignalAndClose(t *testing.T) {
	t.Parallel()
	b := newConsoleInboundBuffer(1024)
	b.append([]byte("x"))
	select {
	case <-b.signal():
	default:
		t.Errorf("signal() not pinged after append")
	}
	b.close()
	if b.append([]byte("y")) {
		t.Errorf("append after close = true, want false")
	}
}
