// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// Local flag names for the join-token subcommand group. Output /
// limit / cursor / arch flags are inherited from common.go.
const (
	flagTTL         = "ttl"
	flagMaxUses     = "max-uses"
	flagNodeName    = "node-name"
	flagKind        = "kind"
	flagIncExpired  = "include-expired"
	defaultTokenTTL = time.Hour
)

// newJoinTokenCommand returns the `otherix node join-token`
// subcommand group, ready for registration on the `node` parent.
func newJoinTokenCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "join-token",
		Short: "Mint, list, and revoke node-agent bootstrap tokens (admin only).",
		Long: `join-token groups the admin-facing subcommands against the Control
Plane's /v1/nodes/join-tokens surface.

Tokens are how a fresh agent process bootstraps its mTLS identity
without manual cert distribution. Step 1 of the bootstrap landing
exposes the management surface (this group); Step 2 lands the
redemption endpoint the agent calls with the token; Steps 3-4 land
the agent-side flow and dev-env migration.

Output on 'create' is the token bundle — plaintext token + active
cluster CA fingerprint — printed exactly once. Save BOTH NOW;
server stores only sha256(token) and plaintext cannot be retrieved.`,
	}
	cmd.AddCommand(newJoinTokenCreateCommand())
	cmd.AddCommand(newJoinTokenListCommand())
	cmd.AddCommand(newJoinTokenRevokeCommand())
	cmd.AddCommand(newJoinTokenConsumptionsCommand())
	return cmd
}

// humanTTLRemaining renders the time-until-expires_at as a short
// "5m", "2h", "3d" string. Past timestamps surface as "expired".
func humanTTLRemaining(rfc3339 string) string {
	if rfc3339 == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339Nano, rfc3339)
	if err != nil {
		t, err = time.Parse(time.RFC3339, rfc3339)
		if err != nil {
			return "-"
		}
	}
	d := time.Until(t)
	if d <= 0 {
		return "expired"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// maxUsesLabel returns "∞" when MaxUses is nil (unlimited) or the
// numeric value. Mirrors the CLI list table convention (Q20 lock).
func maxUsesLabel(maxUses *int64) string {
	if maxUses == nil {
		return "∞"
	}
	return fmt.Sprintf("%d", *maxUses)
}

// nodeNameLabel returns "-" when IntendedNodeName is nil or the
// printable string. Used by the list table.
func nodeNameLabel(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}
