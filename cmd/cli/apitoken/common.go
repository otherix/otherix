// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package apitoken

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// maxTTLDays caps the day component so days*24h cannot overflow int64
// (time.Duration tops out near 292 years). 100 years is already absurd
// for a token lifetime; a larger value is a typo or an overflow attempt.
const maxTTLDays = 36500

// leadingDigits returns the length of the leading run of ASCII digits.
func leadingDigits(s string) int {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i
}

// parseTTL parses a human duration that may carry a leading day
// component (Go's time.ParseDuration has no day unit). Accepted forms:
// "90d", "720h", "30d12h", "45m". The day component, when present, must
// be a LEADING run of digits immediately followed by 'd' - so a 'd'
// buried later (e.g. "12h30d") is rejected, not mis-read as days. The
// result must be positive.
func parseTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration")
	}
	rest := s
	var days time.Duration
	// A day component is a leading integer immediately followed by 'd'.
	if n := leadingDigits(s); n > 0 && n < len(s) && s[n] == 'd' {
		v, err := strconv.Atoi(s[:n])
		if err != nil {
			return 0, fmt.Errorf("invalid day count in %q", s)
		}
		if v > maxTTLDays {
			return 0, fmt.Errorf("ttl too large: %q (max %dd)", s, maxTTLDays)
		}
		days = time.Duration(v) * 24 * time.Hour
		rest = s[n+1:]
	}
	var remainder time.Duration
	if rest != "" {
		d, err := time.ParseDuration(rest)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		remainder = d
	}
	total := days + remainder
	if total <= 0 {
		return 0, fmt.Errorf("duration must be positive: %q", s)
	}
	return total, nil
}

// tokenStatus derives a display status: revoked beats expired beats
// active. Expiry is judged against the local clock. Consumed by the
// list command (a later task).
func tokenStatus(t cpclient.APIToken) string {
	if t.RevokedAt != nil && *t.RevokedAt != "" {
		return "revoked"
	}
	if t.ExpiresAt != nil && *t.ExpiresAt != "" {
		if exp, err := time.Parse(time.RFC3339, *t.ExpiresAt); err == nil && exp.Before(time.Now()) {
			return "expired"
		}
	}
	return "active"
}
