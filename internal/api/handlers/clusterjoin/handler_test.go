// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package clusterjoin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// noopLog returns a discard logger suitable for tests that do not care about
// log output.
func noopLog(_ *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeStore satisfies Store for unit tests.
type fakeStore struct {
	ca        store.CaCert
	redeemErr error
	redeemed  int
}

func (f *fakeStore) ActiveCACert(_ context.Context) (store.CaCert, error) {
	return f.ca, nil
}

func (f *fakeStore) RedeemClusterJoinToken(_ context.Context, _ store.RedeemClusterJoinTokenParams) (store.RedeemClusterJoinResult, error) {
	f.redeemed++
	if f.redeemErr != nil {
		return store.RedeemClusterJoinResult{}, f.redeemErr
	}
	return store.RedeemClusterJoinResult{TokenID: uuid.New()}, nil
}

// fakeMembership satisfies Membership for unit tests.
type fakeMembership struct {
	gotPeerURL string
	members    []etcd.Member
	regErr     error
}

func (f *fakeMembership) RegisterLearner(_ context.Context, peerURL string) ([]etcd.Member, error) {
	f.gotPeerURL = peerURL
	if f.regErr != nil {
		return nil, f.regErr
	}
	return f.members, nil
}

// validToken returns a join token that passes IsJoinTokenFormat.
// "otx_join_" is 9 chars; base64url(32 bytes) = 43 chars => total 52 chars.
func validToken() string { return "otx_join_" + strings.Repeat("a", 43) }

func TestJoinRegistersLearnerAndReturnsMembers(t *testing.T) {
	fs := &fakeStore{
		ca: store.CaCert{
			CertPem:           []byte("CACERT"),
			KeyPem:            []byte("CAKEY"),
			FingerprintSha256: []byte{1, 2},
		},
	}
	fm := &fakeMembership{
		members: []etcd.Member{
			{Name: "n0", PeerURLs: []string{"https://10.0.0.1:2380"}, IsLearner: false},
			{Name: "", PeerURLs: []string{"https://10.0.0.2:2380"}, IsLearner: true},
		},
	}

	h := New(fs, fm, noopLog(t))

	body, _ := json.Marshal(map[string]string{
		"token":    validToken(),
		"peer_url": "https://10.0.0.2:2380",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/cluster/join", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.Join(w, r)

	if w.Code != http.StatusCreated {
		t.Errorf("Join() status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	if fm.gotPeerURL != "https://10.0.0.2:2380" {
		t.Errorf("RegisterLearner got peer_url = %q, want %q", fm.gotPeerURL, "https://10.0.0.2:2380")
	}

	var resp joinResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.CACertPEM != "CACERT" {
		t.Errorf("ca_cert_pem = %q, want %q", resp.CACertPEM, "CACERT")
	}
	if len(resp.Members) != 2 {
		t.Errorf("members len = %d, want 2", len(resp.Members))
	}
	if len(resp.Members) > 0 && resp.Members[0].PeerURL != "https://10.0.0.1:2380" {
		t.Errorf("members[0].peer_url = %q, want %q", resp.Members[0].PeerURL, "https://10.0.0.1:2380")
	}
}

func TestJoinRejectsMissingPeerURL(t *testing.T) {
	fs := &fakeStore{
		ca: store.CaCert{
			CertPem:           []byte("CACERT"),
			KeyPem:            []byte("CAKEY"),
			FingerprintSha256: []byte{1, 2},
		},
	}
	fm := &fakeMembership{}

	h := New(fs, fm, noopLog(t))

	body, _ := json.Marshal(map[string]string{
		"token": validToken(),
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/cluster/join", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.Join(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Join() status = %d, want %d; body: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if fs.redeemed != 0 {
		t.Errorf("redeemed = %d, want 0 (token must not be consumed when peer_url is missing)", fs.redeemed)
	}
}

func TestJoinRegisterLearnerFailure(t *testing.T) {
	fs := &fakeStore{
		ca: store.CaCert{
			CertPem:           []byte("CACERT"),
			KeyPem:            []byte("CAKEY"),
			FingerprintSha256: []byte{1, 2},
		},
	}
	fm := &fakeMembership{regErr: errors.New("etcd unavailable")}

	h := New(fs, fm, noopLog(t))

	body, _ := json.Marshal(map[string]string{
		"token":    validToken(),
		"peer_url": "https://10.0.0.2:2380",
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/cluster/join", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.Join(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("Join() status = %d, want %d; body: %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
}
