// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package jointokens

import (
	"errors"
	"testing"

	"github.com/otherix/otherix/internal/store"
)

func strptr(s string) *string { return &s }

func TestNormaliseCreateRequestKind(t *testing.T) {
	cases := []struct {
		name     string
		req      createRequest
		wantKind string
		wantErr  error
	}{
		{
			name:     "default kind is node",
			req:      createRequest{},
			wantKind: store.JoinTokenKindNode,
		},
		{
			name:     "explicit node",
			req:      createRequest{Kind: strptr("node")},
			wantKind: store.JoinTokenKindNode,
		},
		{
			name:     "explicit cluster",
			req:      createRequest{Kind: strptr("cluster")},
			wantKind: store.JoinTokenKindCluster,
		},
		{
			name:     "empty kind string falls back to node",
			req:      createRequest{Kind: strptr("")},
			wantKind: store.JoinTokenKindNode,
		},
		{
			name:    "unknown kind rejected",
			req:     createRequest{Kind: strptr("peer")},
			wantErr: errInvalidKind,
		},
		{
			name:    "cluster token cannot be node-bound",
			req:     createRequest{Kind: strptr("cluster"), IntendedNodeName: strptr("node-1"), MaxUses: int32ptr(1)},
			wantErr: errClusterNodeBound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, _, _, _, err := normaliseCreateRequest(tc.req)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", kind, tc.wantKind)
			}
		})
	}
}

func int32ptr(v int32) *int32 { return &v }

func TestNormaliseCreateRequestClusterMaxUses(t *testing.T) {
	// A cluster token with no max_uses must default to single-use - it must never
	// be unlimited, since redemption yields the CA private key.
	_, _, _, maxUses, err := normaliseCreateRequest(createRequest{Kind: strptr("cluster")})
	if err != nil {
		t.Fatalf("normaliseCreateRequest: %v", err)
	}
	if maxUses == nil || *maxUses != 1 {
		t.Errorf("cluster max_uses = %v, want 1 (single-use default)", maxUses)
	}

	// An explicit finite cap is preserved (multi-replica grow with one token).
	_, _, _, maxUses, err = normaliseCreateRequest(createRequest{Kind: strptr("cluster"), MaxUses: int32ptr(3)})
	if err != nil {
		t.Fatalf("normaliseCreateRequest explicit: %v", err)
	}
	if maxUses == nil || *maxUses != 3 {
		t.Errorf("cluster explicit max_uses = %v, want 3", maxUses)
	}

	// An explicit cap at the ceiling is allowed.
	_, _, _, maxUses, err = normaliseCreateRequest(createRequest{Kind: strptr("cluster"), MaxUses: int32ptr(maxClusterTokenUses)})
	if err != nil {
		t.Fatalf("normaliseCreateRequest at ceiling: %v", err)
	}
	if maxUses == nil || *maxUses != maxClusterTokenUses {
		t.Errorf("cluster max_uses at ceiling = %v, want %d", maxUses, maxClusterTokenUses)
	}

	// An explicit cap above the ceiling is rejected: a cluster token redeems for the
	// CA private key, so a near-unlimited max_uses defeats the single-use intent.
	_, _, _, _, err = normaliseCreateRequest(createRequest{Kind: strptr("cluster"), MaxUses: int32ptr(maxClusterTokenUses + 1)})
	if !errors.Is(err, errClusterMaxUsesTooHigh) {
		t.Errorf("cluster max_uses above ceiling: err = %v, want %v", err, errClusterMaxUsesTooHigh)
	}

	// A node token is unaffected by the cluster ceiling - leaf certs only.
	_, _, _, maxUses, err = normaliseCreateRequest(createRequest{Kind: strptr("node"), MaxUses: int32ptr(1000)})
	if err != nil {
		t.Fatalf("normaliseCreateRequest node large max_uses: %v", err)
	}
	if maxUses == nil || *maxUses != 1000 {
		t.Errorf("node max_uses = %v, want 1000 (node unaffected)", maxUses)
	}

	// A node token with no max_uses stays unlimited (nil) - unchanged behavior.
	_, _, _, maxUses, err = normaliseCreateRequest(createRequest{Kind: strptr("node")})
	if err != nil {
		t.Fatalf("normaliseCreateRequest node: %v", err)
	}
	if maxUses != nil {
		t.Errorf("node max_uses = %v, want nil (unlimited within TTL)", maxUses)
	}
}
