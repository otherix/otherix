// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"encoding/json"
	"testing"
)

func TestReplicationFactorJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want ReplicationFactor
	}{
		{name: "integer", in: `3`, want: ReplicationFactor{Count: 3}},
		{name: "one", in: `1`, want: ReplicationFactor{Count: 1}},
		{name: "zero parses (validation rejects later)", in: `0`, want: ReplicationFactor{Count: 0}},
		{name: "all sentinel", in: `"all"`, want: ReplicationFactor{All: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got ReplicationFactor
			if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
				t.Fatalf("Unmarshal(%s) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Unmarshal(%s) = %+v, want %+v", tc.in, got, tc.want)
			}
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("Marshal(%+v) error: %v", got, err)
			}
			var rt ReplicationFactor
			if err := json.Unmarshal(b, &rt); err != nil {
				t.Fatalf("re-Unmarshal(%s) error: %v", b, err)
			}
			if rt != tc.want {
				t.Errorf("round-trip = %+v, want %+v", rt, tc.want)
			}
		})
	}

	for _, bad := range []string{`"three"`, `"All"`, `{}`, `[]`, `"1"`} {
		var got ReplicationFactor
		if err := json.Unmarshal([]byte(bad), &got); err == nil {
			t.Errorf("Unmarshal(%s) expected error, got %+v", bad, got)
		}
	}
}
