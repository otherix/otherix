// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package apitoken hosts the `otherix api-token` cobra subcommand group
// and its children over the Control Plane's /v1/users/{me,id}/api-tokens
// surface.
//
// Every authenticated role may manage its own tokens; an admin may
// manage another user's tokens with --user <username> (resolved to a
// user id client-side). The plaintext otx_* token is surfaced exactly
// once, on create, and is never accepted as a flag or argument.
package apitoken

import "github.com/spf13/cobra"

// NewCommand returns the `otherix api-token` subcommand group, ready to
// be registered onto the root cobra tree by main.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "api-token",
		Short: "Manage API tokens (CP /v1/users/.../api-tokens surface)",
		Long: `api-token groups the operator-facing commands for the otx_* API
tokens that authenticate the CLI and automation. Every authenticated
role manages its own tokens; an admin may act on another user's tokens
with --user <username>.

The plaintext token is shown exactly once, on create, and is never
passed as a flag or argument.`,
	}
	cmd.AddCommand(newCreateCommand())
	return cmd
}
