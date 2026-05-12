// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

func TestScanForbiddenKeys(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body []byte
		keys []string
		want []string
	}{
		{
			name: "empty body returns nil",
			body: nil,
			keys: []string{"id", "created_at"},
		},
		{
			name: "whitespace body returns nil",
			body: []byte("   \n\t  "),
			keys: []string{"id"},
		},
		{
			name: "non-object body returns nil",
			body: []byte(`["a","b"]`),
			keys: []string{"id"},
		},
		{
			name: "unparseable body returns nil",
			body: []byte(`{not json`),
			keys: []string{"id"},
		},
		{
			name: "no forbidden keys present",
			body: []byte(`{"name":"x","type":"local_dir"}`),
			keys: []string{"id", "created_at"},
		},
		{
			name: "single forbidden key present",
			body: []byte(`{"name":"x","id":"u"}`),
			keys: []string{"id", "created_at"},
			want: []string{"id"},
		},
		{
			name: "all forbidden keys present preserves keys order",
			body: []byte(`{"id":"u","created_at":"t","name":"x"}`),
			keys: []string{"id", "created_at"},
			want: []string{"id", "created_at"},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ScanForbiddenKeys(tc.body, tc.keys)
			if diff := cmp.Diff(tc.want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ScanForbiddenKeys mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
