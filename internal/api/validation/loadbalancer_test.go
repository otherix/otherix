// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package validation_test

import (
	"testing"

	"github.com/otherix/otherix/internal/api/validation"
)

func TestValidateLBProtocol(t *testing.T) {
	if err := validation.ValidateLBProtocol("tcp"); err != nil {
		t.Errorf("ValidateLBProtocol(tcp) = %v, want nil", err)
	}
	for _, bad := range []string{"udp", "TCP", "http", ""} {
		if err := validation.ValidateLBProtocol(bad); err == nil {
			t.Errorf("ValidateLBProtocol(%q) = nil, want error", bad)
		}
	}
}

func TestValidateSourceCIDR(t *testing.T) {
	for _, ok := range []string{"203.0.113.0/24", "10.0.0.1/32", "0.0.0.0/0", "2001:db8::/32"} {
		if err := validation.ValidateSourceCIDR(ok); err != nil {
			t.Errorf("ValidateSourceCIDR(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"203.0.113.0", "not-a-cidr", "10.0.0.0/33", ""} {
		if err := validation.ValidateSourceCIDR(bad); err == nil {
			t.Errorf("ValidateSourceCIDR(%q) = nil, want error", bad)
		}
	}
}
