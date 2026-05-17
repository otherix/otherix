// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliconfig"
)

func newUseCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Set the current cluster",
		Args:  cobra.ExactArgs(1),
		RunE:  runUse,
	}
}

func runUse(cmd *cobra.Command, args []string) error {
	name := args[0]
	path, err := resolveConfigPath(cmd)
	if err != nil {
		return err
	}
	cfg, err := cliconfig.Load(path)
	if err != nil {
		return err
	}
	if err := cfg.SetCurrent(name); err != nil {
		if errors.Is(err, cliconfig.ErrClusterNotFound) {
			return fmt.Errorf("cluster %q not in config — run `otherix config list`", name)
		}
		return err
	}
	if err := cliconfig.Save(path, cfg); err != nil {
		return err
	}
	printf(cmd.OutOrStdout(), "current cluster: %s\n", name)
	return nil
}
