// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package forward

import (
	"strings"
	"testing"
)

// TestResolveForwardTarget covers the kubectl-style port-spec resolution: the
// bind address comes from --address (or the -L/--listen shortcut), and the
// [LOCAL:]REMOTE positional supplies the local and guest ports.
func TestResolveForwardTarget(t *testing.T) {
	tests := []struct {
		name       string
		portSpec   string
		address    string
		addressSet bool
		listen     string
		listenSet  bool
		wantListen string
		wantGuest  int
	}{
		{
			name:       "bare remote binds local equal to remote",
			portSpec:   "5432",
			address:    "127.0.0.1",
			listen:     "127.0.0.1:0",
			wantListen: "127.0.0.1:5432",
			wantGuest:  5432,
		},
		{
			name:       "explicit local and remote",
			portSpec:   "15432:5432",
			address:    "127.0.0.1",
			listen:     "127.0.0.1:0",
			wantListen: "127.0.0.1:15432",
			wantGuest:  5432,
		},
		{
			name:       "empty local requests an ephemeral port",
			portSpec:   ":5432",
			address:    "127.0.0.1",
			listen:     "127.0.0.1:0",
			wantListen: "127.0.0.1:0",
			wantGuest:  5432,
		},
		{
			name:       "address flag sets the bind host for a bare remote",
			portSpec:   "5432",
			address:    "0.0.0.0",
			addressSet: true,
			listen:     "127.0.0.1:0",
			wantListen: "0.0.0.0:5432",
			wantGuest:  5432,
		},
		{
			name:       "address flag composes with an explicit local port",
			portSpec:   "15432:5432",
			address:    "0.0.0.0",
			addressSet: true,
			listen:     "127.0.0.1:0",
			wantListen: "0.0.0.0:15432",
			wantGuest:  5432,
		},
		{
			name:       "ipv6 address is bracketed",
			portSpec:   "5432",
			address:    "::1",
			addressSet: true,
			listen:     "127.0.0.1:0",
			wantListen: "[::1]:5432",
			wantGuest:  5432,
		},
		{
			name:       "listen shortcut wins with a bare remote (backward compatible)",
			portSpec:   "5432",
			address:    "127.0.0.1",
			listen:     "127.0.0.1:9000",
			listenSet:  true,
			wantListen: "127.0.0.1:9000",
			wantGuest:  5432,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveForwardTarget(tt.portSpec, tt.address, tt.addressSet, tt.listen, tt.listenSet)
			if err != nil {
				t.Fatalf("resolveForwardTarget(%q, %q, %v, %q, %v) unexpected error: %v",
					tt.portSpec, tt.address, tt.addressSet, tt.listen, tt.listenSet, err)
			}
			if got.listenAddr != tt.wantListen || got.guestPort != tt.wantGuest {
				t.Errorf("resolveForwardTarget(%q, ...) = {listen:%q guest:%d}, want {listen:%q guest:%d}",
					tt.portSpec, got.listenAddr, got.guestPort, tt.wantListen, tt.wantGuest)
			}
		})
	}
}

// TestResolveForwardTargetErrors covers the fail-loud paths: malformed specs and
// the mutually-exclusive ways to set the local bind.
func TestResolveForwardTargetErrors(t *testing.T) {
	tests := []struct {
		name       string
		portSpec   string
		address    string
		addressSet bool
		listen     string
		listenSet  bool
		wantSubstr string
	}{
		{
			name:       "address and listen both set",
			portSpec:   "5432",
			address:    "0.0.0.0",
			addressSet: true,
			listen:     "127.0.0.1:9000",
			listenSet:  true,
			wantSubstr: "--address",
		},
		{
			name:       "listen set with an explicit local port in the spec",
			portSpec:   "15432:5432",
			address:    "127.0.0.1",
			listen:     "127.0.0.1:9000",
			listenSet:  true,
			wantSubstr: "--listen",
		},
		{
			name:       "address embedded in the port spec",
			portSpec:   "0.0.0.0:15432:5432",
			address:    "127.0.0.1",
			listen:     "127.0.0.1:0",
			wantSubstr: "--address",
		},
		{
			name:       "remote port out of range",
			portSpec:   "70000",
			address:    "127.0.0.1",
			listen:     "127.0.0.1:0",
			wantSubstr: "65535",
		},
		{
			name:       "remote port zero",
			portSpec:   "0",
			address:    "127.0.0.1",
			listen:     "127.0.0.1:0",
			wantSubstr: "65535",
		},
		{
			name:       "empty spec",
			portSpec:   "",
			address:    "127.0.0.1",
			listen:     "127.0.0.1:0",
			wantSubstr: "port",
		},
		{
			name:       "non-numeric local port",
			portSpec:   "abc:5432",
			address:    "127.0.0.1",
			listen:     "127.0.0.1:0",
			wantSubstr: "local port",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveForwardTarget(tt.portSpec, tt.address, tt.addressSet, tt.listen, tt.listenSet)
			if err == nil {
				t.Fatalf("resolveForwardTarget(%q, ...) = nil error, want error containing %q", tt.portSpec, tt.wantSubstr)
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("resolveForwardTarget(%q, ...) error = %q, want substring %q", tt.portSpec, err.Error(), tt.wantSubstr)
			}
		})
	}
}
