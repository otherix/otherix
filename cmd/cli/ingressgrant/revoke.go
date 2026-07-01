// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package ingressgrant

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"
)

// newRevokeCommand returns `otherix ingress-grant revoke <id|name>`.
func newRevokeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "revoke <id|name>",
		Short: "Revoke an ingress grant.",
		Long: `Revokes an ingress grant by id or name. The grant's token stops working
immediately. Revoking an already-revoked grant is a no-op.`,
		Args: cobra.ExactArgs(1),
		RunE: runRevoke,
	}
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json|yaml")
	return cmd
}

func runRevoke(cmd *cobra.Command, args []string) error {
	identifier := strings.TrimSpace(args[0])
	if identifier == "" {
		return errors.New("id or name is required")
	}
	format, err := outputFormat(cmd, "text", "yaml")
	if err != nil {
		return err
	}
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	grant, err := c.RevokeIngressGrant(cmd.Context(), identifier)
	if err != nil {
		return mapGrantError(err, identifier)
	}
	if format == "text" {
		printf(cmd, "ingress grant %q revoked\n", grant.Name)
		return nil
	}
	return renderGrant(cmd, grant, nil, format)
}
