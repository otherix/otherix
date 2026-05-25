// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// newJoinTokenRevokeCommand builds `otherix node join-token revoke
// <id>`.
func newJoinTokenRevokeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke an unconsumed join token (admin only).",
		Long: `Sets the token's expires_at to LEAST(expires_at, now()), making
it un-redeemable. Idempotent at the server (repeat calls produce
no further effect); already-expired tokens return 409.`,
		Args: cobra.ExactArgs(1),
		RunE: runJoinTokenRevoke,
	}
	return cmd
}

func runJoinTokenRevoke(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	id, err := uuid.Parse(args[0])
	if err != nil {
		return fmt.Errorf("validation_failed: id must be a uuid: %v", err)
	}
	if err := c.DeleteJoinToken(cmd.Context(), id); err != nil {
		return classifyError(err)
	}
	printf(cmd, "token %s revoked\n", id)
	return nil
}
