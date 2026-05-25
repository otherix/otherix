// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliconfig"
)

func newListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured clusters",
		RunE:  runList,
	}
}

func runList(cmd *cobra.Command, _ []string) error {
	path, err := resolveConfigPath(cmd)
	if err != nil {
		return err
	}
	cfg, err := cliconfig.Load(path)
	if err != nil {
		return err
	}
	if len(cfg.Clusters) == 0 {
		printf(cmd.OutOrStdout(), "no clusters configured (run `otherix config add cluster`)\n")
		return nil
	}
	writeClusterTable(cmd.OutOrStdout(), cfg)
	return nil
}

// writeClusterTable renders the cluster list as a tabwriter-aligned
// three-column table — NAME / SERVER / CURRENT. The current
// cluster is marked with `*` in the third column.
func writeClusterTable(w io.Writer, cfg *cliconfig.Config) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tSERVER\tCURRENT")
	for _, c := range cfg.Clusters {
		mark := ""
		if c.Name == cfg.CurrentCluster {
			mark = "*"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Name, c.Server, mark)
	}
	_ = tw.Flush()
}
