// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package user hosts the `otherix user` cobra subcommand group and its
// children - the CRUD surface (create / get / list / delete) plus the
// role / password mutators (set-role / set-password) and the
// self-introspection command (whoami) over the Control Plane's
// /v1/users endpoints.
//
// A username is the primary, immutable, unique identity: it is the
// positional argument on create / get / delete / set-role / set-password,
// and the only login credential. Email and display_name are optional.
//
// The CP GET-by-id / PATCH / DELETE routes accept ONLY a UUID literal.
// To keep `get`/`delete`/`set-role`/`set-password <username>` ergonomic
// for operators, the cpclient resolves a username to its UUID
// client-side via a guard-backed list-then-match before issuing the
// by-id roundtrip.
//
// Passwords are never accepted as a command argument or a flag value
// (that would leak into shell history / the process table). create and
// set-password read the password either from an interactive no-echo
// terminal prompt or, for automation, one line from stdin via
// --password-stdin.
package user

import "github.com/spf13/cobra"

// NewCommand returns the `otherix user` subcommand group, ready to be
// registered onto the root cobra tree by main.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage users (CP /v1/users surface)",
		Long: `user groups the operator-facing commands against the Control
Plane's /v1/users surface. A username is the primary immutable identity
and the only login credential; email and display_name are optional.
Creating, listing, deleting, and re-roling users is admin-only
(user:manage); 'whoami' is available to every authenticated role.

Passwords are never passed as a flag or argument - 'create' and
'set-password' read the password from an interactive no-echo prompt or
from stdin via --password-stdin.`,
	}
	cmd.AddCommand(newCreateCommand())
	cmd.AddCommand(newGetCommand())
	cmd.AddCommand(newListCommand())
	cmd.AddCommand(newDeleteCommand())
	cmd.AddCommand(newSetRoleCommand())
	cmd.AddCommand(newSetPasswordCommand())
	cmd.AddCommand(newWhoamiCommand())
	return cmd
}
