// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package apitoken

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// newRevokeCommand returns the `otherix api-token revoke` cobra command.
// The positional argument is a token prefix (otx_xxxx, resolved
// client-side to its id) or a full token id (used directly). --user
// targets another user's tokens (admin-only).
func newRevokeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "revoke <prefix-or-id>",
		Short: "Revoke an API token by prefix (or full token id).",
		Long: `Revokes an API token. The argument is normally a prefix (the
otx_xxxx shown by 'api-token list'), resolved to its id client-side. If
the prefix is ambiguous (more than one match), pass the full token id
instead. --user (admin-only) targets another user's tokens.

Revoke is immediate and idempotent.

  otherix api-token revoke otx_ab12
  otherix api-token revoke otx_ab12 --user alice --force`,
		Args: cobra.ExactArgs(1),
		RunE: runRevoke,
	}
	cmd.Flags().String(flagUser, "", "act on this user's tokens (admin-only); default: yourself")
	cmd.Flags().Bool(flagForce, false, "skip the confirmation prompt")
	cmd.Flags().String(flagOutput, "text", "output format: text|json")
	return cmd
}

func runRevoke(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	arg := args[0]
	if arg == "" {
		return errors.New("a token prefix or id is required")
	}
	username, _ := cmd.Flags().GetString(flagUser)
	force, _ := cmd.Flags().GetBool(flagForce)
	format, err := outputFormat(cmd, "text")
	if err != nil {
		return err
	}

	userID, err := resolveTargetUserID(cmd.Context(), c, username)
	if err != nil {
		return err
	}

	tok, err := resolveTokenByPrefix(cmd.Context(), c, userID, arg)
	if err != nil {
		return err
	}

	// Already-revoked prefix match: report the no-op, do not re-delete.
	// (The full-id escape hatch carries no revoked_at, so it falls
	// through to the idempotent DELETE below.)
	if tok.RevokedAt != nil && *tok.RevokedAt != "" {
		printf(cmd, "api token %s already revoked\n", arg)
		return nil
	}

	if !force && stdinIsTTY() {
		printf(cmd, "revoke api token %s? [y/N]: ", arg)
		if !readYes() {
			printf(cmd, "aborted\n")
			return nil
		}
	}

	if err := c.RevokeAPITokenFor(cmd.Context(), userID, tok.ID); err != nil {
		return classifyError(err)
	}

	label := arg
	if tok.Name != "" {
		label = fmt.Sprintf("%s (%s)", arg, tok.Name)
	}
	switch format {
	case "json":
		printf(cmd, "{\"revoked\":true,\"token\":%q}\n", arg)
	default:
		printf(cmd, "api token %s revoked\n", label)
	}
	return nil
}
