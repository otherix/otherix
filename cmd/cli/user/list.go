// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package user

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func newListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List users (admin-only).",
		Long: `Cursor-paginated list of users. Required permission: user:manage
(admin role). Optional server filter: --username (exact match).`,
		RunE: runList,
	}
	cmd.Flags().String("username", "", "filter by exact username")
	cmd.Flags().Int(flagLimit, defaultListLimit, "page size (1..200)")
	cmd.Flags().String(flagCursor, "", "opaque cursor from a previous page")
	cmd.Flags().StringP(flagOutput, "o", "table", "output format: table|json|yaml")
	return cmd
}

func runList(cmd *cobra.Command, _ []string) error {
	c, err := clientFromFlags(cmd)
	if err != nil {
		return err
	}
	username, _ := cmd.Flags().GetString("username")
	limit, _ := cmd.Flags().GetInt(flagLimit)
	cursor, _ := cmd.Flags().GetString(flagCursor)
	format, err := outputFormat(cmd, "table", "yaml")
	if err != nil {
		return err
	}

	users, err := c.ListUsers(cmd.Context(), cpclient.ListUsersParams{
		Username: username,
		Limit:    limit,
		Cursor:   cursor,
	})
	if err != nil {
		return classifyError(err)
	}

	switch format {
	case "json":
		raw, err := json.MarshalIndent(users, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	case "yaml":
		out, err := yaml.Marshal(users)
		if err != nil {
			return fmt.Errorf("marshal yaml: %v", err)
		}
		printf(cmd, "%s", out)
	default:
		printUserTable(cmd, users)
	}
	return nil
}

func printUserTable(cmd *cobra.Command, users cpclient.UserList) {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "USERNAME\tROLE\tEMAIL\tDISPLAY_NAME\tCREATED_AT")
	for _, u := range users.Data {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			u.Username, u.Role, dash(u.Email), dash(u.DisplayName), u.CreatedAt)
	}
	_ = tw.Flush()
	if users.Meta.NextCursor != nil {
		printNextCursor(cmd, *users.Meta.NextCursor)
	}
}
