// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package apitoken

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/clierr"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagOutput = "output"
	flagUser   = "user"
	flagTTL    = "ttl"
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
// active. Expiry is judged against the local clock.
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

func clientFromFlags(cmd *cobra.Command) (*cpclient.Client, error) {
	return cliauth.BuildClient(cmd)
}

func printf(cmd *cobra.Command, format string, args ...any) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), format, args...)
}

// classifyError maps a cpclient error to a stable-prefixed operator
// message (shared with the other CLI surfaces).
func classifyError(err error) error {
	return clierr.Classify(err)
}

// outputFormat reads --output (default defaultFormat). text/json/table
// are always accepted; extra opts into more (yaml). Unknown -> usage error.
func outputFormat(cmd *cobra.Command, defaultFormat string, extra ...string) (string, error) {
	raw, err := cmd.Flags().GetString(flagOutput)
	if err != nil {
		return "", err
	}
	if raw == "" {
		raw = defaultFormat
	}
	allowed := append([]string{"text", "json", "table"}, extra...)
	if slices.Contains(allowed, raw) {
		return raw, nil
	}
	return "", fmt.Errorf("--%s: unknown format %q (%s)", flagOutput, raw, strings.Join(allowed, ", "))
}

func derefOr(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}
	return *p
}

// resolveTargetUserID maps the --user flag to a target user id for the
// admin-on-behalf paths. An empty username yields "" (the self path).
//
// The username lookup goes through GET /v1/users (user:read), which only
// admin and operator hold. A developer/viewer using --user therefore
// gets a 403 from the lookup itself, before ever reaching the apitokens
// route. Both a not-found (empty page) and a permission_denied collapse
// to the same clean "not found or not permitted" message so the operator
// is not shown a confusing "permission_denied on users list" for a call
// they never made, and so existence is not leaked across the boundary.
func resolveTargetUserID(ctx context.Context, c *cpclient.Client, username string) (string, error) {
	if username == "" {
		return "", nil
	}
	u, err := c.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, cpclient.ErrNotFound) {
			return "", fmt.Errorf("user %q not found or not permitted", username)
		}
		var apiErr *cpclient.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.Code == "permission_denied") {
			return "", fmt.Errorf("user %q not found or not permitted (admin-only operation)", username)
		}
		return "", classifyError(err)
	}
	return u.ID, nil
}

// renderToken writes a single token view in json/yaml; the text branch
// prints a compact identity block.
func renderToken(cmd *cobra.Command, t cpclient.APIToken, format string) error {
	switch format {
	case "json":
		raw, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	case "yaml":
		out, err := yaml.Marshal(t)
		if err != nil {
			return fmt.Errorf("marshal yaml: %v", err)
		}
		printf(cmd, "%s", out)
	default:
		printf(cmd, "id: %s\nname: %s\nprefix: %s\nstatus: %s\n", t.ID, t.Name, t.Prefix, tokenStatus(t))
	}
	return nil
}
