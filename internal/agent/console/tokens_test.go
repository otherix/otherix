// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package console

import (
	"errors"
	"testing"
	"time"
)

func TestTokenStore_IssueConsumeRoundTrip(t *testing.T) {
	t.Parallel()
	s := NewTokenStore()

	raw, expiresAt, err := s.Issue("vm-1", ProtocolSerial)
	if err != nil {
		t.Fatalf("Issue(vm-1, serial) = %v", err)
	}
	if raw == "" {
		t.Fatal("Issue returned empty raw token")
	}
	if expiresAt.Before(time.Now()) {
		t.Errorf("Issue ExpiresAt %v should be in the future", expiresAt)
	}

	got, err := s.Consume(raw, "vm-1")
	if err != nil {
		t.Fatalf("Consume(valid) = %v", err)
	}
	if got.VMName != "vm-1" || got.Protocol != ProtocolSerial {
		t.Errorf("Consume returned %+v, want vm=vm-1 proto=serial", got)
	}
}

func TestTokenStore_ConsumeSingleUse(t *testing.T) {
	t.Parallel()
	s := NewTokenStore()
	raw, _, err := s.Issue("vm-1", ProtocolSerial)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if _, err := s.Consume(raw, "vm-1"); err != nil {
		t.Fatalf("first Consume: %v", err)
	}
	_, err = s.Consume(raw, "vm-1")
	if !errors.Is(err, ErrTokenConsumed) {
		t.Errorf("second Consume = %v, want ErrTokenConsumed", err)
	}
}

func TestTokenStore_ConsumeRejectsUnknownToken(t *testing.T) {
	t.Parallel()
	s := NewTokenStore()
	_, err := s.Consume("totally-not-issued", "vm-1")
	if !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("Consume(unknown) = %v, want ErrTokenNotFound", err)
	}
}

func TestTokenStore_ConsumeRejectsEmptyToken(t *testing.T) {
	t.Parallel()
	s := NewTokenStore()
	_, err := s.Consume("", "vm-1")
	if !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("Consume(\"\") = %v, want ErrTokenNotFound", err)
	}
}

func TestTokenStore_ConsumeChecksVMBinding(t *testing.T) {
	t.Parallel()
	s := NewTokenStore()
	raw, _, err := s.Issue("vm-a", ProtocolSerial)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	_, err = s.Consume(raw, "vm-b")
	if !errors.Is(err, ErrTokenVMMismatch) {
		t.Errorf("Consume(wrong vm) = %v, want ErrTokenVMMismatch", err)
	}
}

func TestTokenStore_ConsumeRejectsExpired(t *testing.T) {
	t.Parallel()
	now := time.Now()
	s := NewTokenStore()
	s.now = func() time.Time { return now }
	s.ttl = time.Second

	raw, _, err := s.Issue("vm-1", ProtocolSerial)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	s.now = func() time.Time { return now.Add(2 * time.Second) }

	_, err = s.Consume(raw, "vm-1")
	if !errors.Is(err, ErrTokenExpired) {
		t.Errorf("Consume(expired) = %v, want ErrTokenExpired", err)
	}
	if s.Size() != 0 {
		t.Errorf("expired entry not removed on Consume — Size()=%d", s.Size())
	}
}

func TestTokenStore_IssueRejectsBadInput(t *testing.T) {
	t.Parallel()
	s := NewTokenStore()
	if _, _, err := s.Issue("", ProtocolSerial); err == nil {
		t.Error("Issue(\"\", serial) should reject empty vm name")
	}
	if _, _, err := s.Issue("vm-1", Protocol("rdp")); err == nil {
		t.Error("Issue(vm-1, rdp) should reject unsupported protocol")
	}
	if _, _, err := s.Issue("vm-1", Protocol("")); err == nil {
		t.Error("Issue(vm-1, zero-protocol) should reject empty protocol")
	}
}

func TestTokenStore_SweepExpiredRemovesConsumedAndExpired(t *testing.T) {
	t.Parallel()
	now := time.Now()
	s := NewTokenStore()
	s.now = func() time.Time { return now }
	s.ttl = time.Second

	rawConsumed, _, err := s.Issue("vm-a", ProtocolSerial)
	if err != nil {
		t.Fatalf("Issue(a): %v", err)
	}
	if _, err := s.Consume(rawConsumed, "vm-a"); err != nil {
		t.Fatalf("Consume(a): %v", err)
	}

	if _, _, err := s.Issue("vm-b", ProtocolSerial); err != nil {
		t.Fatalf("Issue(b): %v", err)
	}

	rawFresh, _, err := s.Issue("vm-c", ProtocolSerial)
	if err != nil {
		t.Fatalf("Issue(c): %v", err)
	}

	s.now = func() time.Time { return now.Add(2 * time.Second) }
	s.SweepExpired()

	if got := s.Size(); got != 0 {
		t.Errorf("after sweep Size()=%d, want 0 (consumed+expired both removed)", got)
	}

	// Once swept the entry is gone; Consume distinguishes "not in store"
	// от "in store but expired" only до the next sweep. Both surface as
	// 401 в the handler — the test just locks the post-sweep shape.
	if _, err := s.Consume(rawFresh, "vm-c"); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("Consume(post-sweep) = %v, want ErrTokenNotFound (entry was swept)", err)
	}
}
