// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation

import (
	"testing"

	"github.com/otherix/otherix/internal/store"
)

func TestValidateArtifactPoolName(t *testing.T) {
	good := []string{"gold", "bronze-1", "a.b_c", "x"}
	for _, n := range good {
		if err := ValidateArtifactPoolName(n); err != nil {
			t.Errorf("ValidateArtifactPoolName(%q) = %v, want nil", n, err)
		}
	}
	bad := []string{"", "has/slash", "../x", "-leading", "white space"}
	for _, n := range bad {
		if err := ValidateArtifactPoolName(n); err == nil {
			t.Errorf("ValidateArtifactPoolName(%q) = nil, want error", n)
		}
	}
}

func TestValidateReplicationFactor(t *testing.T) {
	if err := ValidateReplicationFactor(store.ReplicationFactor{All: true}); err != nil {
		t.Errorf("all = %v, want nil", err)
	}
	if err := ValidateReplicationFactor(store.ReplicationFactor{Count: 1}); err != nil {
		t.Errorf("1 = %v, want nil", err)
	}
	for _, c := range []int32{0, -1} {
		if err := ValidateReplicationFactor(store.ReplicationFactor{Count: c}); err == nil {
			t.Errorf("ValidateReplicationFactor(%d) = nil, want error", c)
		}
	}
}
