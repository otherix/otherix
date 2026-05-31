// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

type stubCertQuerier struct {
	row auth.AgentCertLookupResult
	err error
}

func (s *stubCertQuerier) LookupAgentCertByFingerprint(_ context.Context, _ []byte) (auth.AgentCertLookupResult, error) {
	return s.row, s.err
}

func TestVerifyFingerprint_Success(t *testing.T) {
	want := uuid.New()
	v := auth.NewAgentVerifier(&stubCertQuerier{
		row: auth.AgentCertLookupResult{NodeID: want, Revoked: false},
	})
	got, err := v.VerifyFingerprint(context.Background(), []byte("fp"))
	if err != nil {
		t.Fatalf("VerifyFingerprint: %v", err)
	}
	if got.NodeID != want {
		t.Errorf("NodeID = %v, want %v", got.NodeID, want)
	}
	if string(got.CertFingerprint) != "fp" {
		t.Errorf("CertFingerprint = %q, want fp", got.CertFingerprint)
	}
}

func TestVerifyFingerprint_Unknown(t *testing.T) {
	v := auth.NewAgentVerifier(&stubCertQuerier{err: store.ErrNotFound})
	_, err := v.VerifyFingerprint(context.Background(), []byte("fp"))
	if !errors.Is(err, auth.ErrCertUnknown) {
		t.Errorf("err = %v, want ErrCertUnknown", err)
	}
}

func TestVerifyFingerprint_UnknownThroughWrap(t *testing.T) {
	wrapped := wrap("agent cert lookup", store.ErrNotFound)
	v := auth.NewAgentVerifier(&stubCertQuerier{err: wrapped})
	_, err := v.VerifyFingerprint(context.Background(), []byte("fp"))
	if !errors.Is(err, auth.ErrCertUnknown) {
		t.Errorf("err = %v, want ErrCertUnknown via errors.Is through wrap", err)
	}
}

func TestVerifyFingerprint_Revoked(t *testing.T) {
	v := auth.NewAgentVerifier(&stubCertQuerier{
		row: auth.AgentCertLookupResult{NodeID: uuid.New(), Revoked: true},
	})
	_, err := v.VerifyFingerprint(context.Background(), []byte("fp"))
	if !errors.Is(err, auth.ErrCertRevoked) {
		t.Errorf("err = %v, want ErrCertRevoked", err)
	}
}

func TestVerifyFingerprint_DBError(t *testing.T) {
	want := errors.New("connection refused")
	v := auth.NewAgentVerifier(&stubCertQuerier{err: want})
	_, err := v.VerifyFingerprint(context.Background(), []byte("fp"))
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want connection-refused passthrough", err)
	}
}

// wrap is the local equivalent of fmt.Errorf("...: %w", err) used to
// confirm errors.Is travels the wrap chain. Avoids fmt to keep this
// test free of unused-import noise once the helper is the only call
// site.
func wrap(prefix string, err error) error {
	return &wrappedErr{prefix: prefix, err: err}
}

type wrappedErr struct {
	prefix string
	err    error
}

func (w *wrappedErr) Error() string { return w.prefix + ": " + w.err.Error() }
func (w *wrappedErr) Unwrap() error { return w.err }
