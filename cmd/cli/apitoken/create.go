// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package apitoken

import (
	"errors"
	"time"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// newCreateCommand returns the `otherix api-token create` cobra command.
// The positional <name> labels the token. --ttl sets a relative lifetime
// (default: never expires). --user mints on behalf of another user
// (admin-only). The plaintext token is printed once on success and is
// never an input flag or argument.
func newCreateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Mint an API token.",
		Long: `Mints an otx_* API token under your account, or - with --user
(admin-only) - under another user's account. The token carries the
owner's role; permissions resolve at request time.

--ttl sets a relative lifetime (90d, 720h, 30d12h); omit it for a
long-lived token. The plaintext token is shown exactly once on success;
copy it immediately. It is never accepted as a flag or argument.

  otherix api-token create ci-bot
  otherix api-token create ci-bot --ttl 90d
  otherix api-token create deploy --user alice   # admin-on-behalf`,
		Args: cobra.ExactArgs(1),
		RunE: runCreate,
	}
	cmd.Flags().String(flagTTL, "", "lifetime as a duration (90d, 720h, 30d12h); default: never expires")
	cmd.Flags().String(flagUser, "", "act on this user's tokens (admin-only); default: yourself")
	cmd.Flags().StringP(flagOutput, "o", "text", "output format: text|json|yaml")
	return cmd
}

func runCreate(cmd *cobra.Command, args []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	name := args[0]
	if name == "" {
		return errors.New("name is required")
	}
	username, _ := cmd.Flags().GetString(flagUser)
	ttlStr, _ := cmd.Flags().GetString(flagTTL)
	format, err := outputFormat(cmd, "text", "yaml")
	if err != nil {
		return err
	}

	req := cpclient.CreateAPITokenRequest{Name: name}
	if ttlStr != "" {
		ttl, err := parseTTL(ttlStr)
		if err != nil {
			return err
		}
		exp := time.Now().UTC().Add(ttl).Format(time.RFC3339)
		req.ExpiresAt = &exp
	}

	userID, err := resolveTargetUserID(cmd.Context(), c, username)
	if err != nil {
		return err
	}

	created, err := c.CreateAPITokenFor(cmd.Context(), userID, req)
	if err != nil {
		return classifyError(err)
	}
	return renderCreated(cmd, created, format)
}

// renderCreated prints the create result. text prints the plaintext
// prominently with a shown-once warning, then the metadata. json/yaml
// emit the full create view (token included), for capture by automation.
func renderCreated(cmd *cobra.Command, t cpclient.APIToken, format string) error {
	if format != "text" {
		return renderToken(cmd, t, format)
	}
	printf(cmd, "api token created - copy it now, it will not be shown again:\n\n")
	printf(cmd, "  %s\n\n", t.Token)
	printf(cmd, "  name:       %s\n", t.Name)
	printf(cmd, "  prefix:     %s\n", t.Prefix)
	printf(cmd, "  expires_at: %s\n", derefOr(t.ExpiresAt, "never"))
	printf(cmd, "  id:         %s\n", t.ID)
	return nil
}
