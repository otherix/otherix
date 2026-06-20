// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import "testing"

func TestParseImagePullPolicy(t *testing.T) {
	tests := []struct {
		in      string
		want    ImagePullPolicy
		wantErr bool
	}{
		{"if_not_present", ImagePullPolicyIfNotPresent, false},
		{"always", ImagePullPolicyAlways, false},
		{"", ImagePullPolicyIfNotPresent, false}, // empty defaults to if_not_present
		{"Always", "", true},
		{"bogus", "", true},
	}
	for _, tt := range tests {
		got, err := ParseImagePullPolicy(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseImagePullPolicy(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if err == nil && got != tt.want {
			t.Errorf("ParseImagePullPolicy(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
