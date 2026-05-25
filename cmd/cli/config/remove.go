// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"bufio"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliconfig"
)

func newRemoveCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a cluster from the config",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRemove(cmd, args[0], force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "skip the interactive confirmation prompt")
	return cmd
}

func runRemove(cmd *cobra.Command, name string, force bool) error {
	path, err := resolveConfigPath(cmd)
	if err != nil {
		return err
	}
	cfg, err := cliconfig.Load(path)
	if err != nil {
		return err
	}
	if _, err := cfg.FindCluster(name); err != nil {
		if errors.Is(err, cliconfig.ErrClusterNotFound) {
			return fmt.Errorf("cluster %q not in config", name)
		}
		return err
	}
	if !force {
		if !stdinIsTerminal() {
			return errors.New("non-interactive remove requires --force")
		}
		reader := bufio.NewReader(cmd.InOrStdin())
		ok, err := confirmYN(cmd.ErrOrStderr(), reader, fmt.Sprintf("Remove cluster %q?", name))
		if err != nil {
			return err
		}
		if !ok {
			printf(cmd.OutOrStdout(), "aborted\n")
			return nil
		}
	}
	wasCurrent := cfg.CurrentCluster == name
	if err := cfg.RemoveCluster(name); err != nil {
		return err
	}
	if err := cliconfig.Save(path, cfg); err != nil {
		return err
	}
	printf(cmd.OutOrStdout(), "cluster removed: %s\n", name)
	if wasCurrent && cfg.CurrentCluster == "" {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(),
			"warning: current-cluster cleared; run `otherix config add cluster` to set up")
	} else if wasCurrent {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "current cluster advanced to: %s\n", cfg.CurrentCluster)
	}
	return nil
}
