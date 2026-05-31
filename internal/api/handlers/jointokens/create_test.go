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
