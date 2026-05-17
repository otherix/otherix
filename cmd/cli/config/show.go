// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliconfig"
)

func newShowCommand() *cobra.Command {
	var showToken bool
	cmd := &cobra.Command{
		Use:   "show [name]",
		Short: "Print а cluster's details (token masked unless --show-token)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return runShow(cmd, name, showToken)
		},
	}
	cmd.Flags().BoolVar(&showToken, "show-token", false, "reveal the plaintext API token")
	return cmd
}

func runShow(cmd *cobra.Command, name string, showToken bool) error {
	path, err := resolveConfigPath(cmd)
	if err != nil {
		return err
	}
	cfg, err := cliconfig.Load(path)
	if err != nil {
		return err
	}
	if name == "" {
		entry, err := cfg.CurrentClusterEntry()
		if err != nil {
			if errors.Is(err, cliconfig.ErrEmptyConfig) {
				return errors.New("no clusters configured: run `otherix config add cluster`")
			}
			if errors.Is(err, cliconfig.ErrClusterNotFound) {
				return fmt.Errorf("current-cluster %q missing from config — run `otherix config use <name>`", cfg.CurrentCluster)
			}
			return err
		}
		writeClusterDetail(cmd, entry, true, showToken)
		return nil
	}
	entry, err := cfg.FindCluster(name)
	if err != nil {
		if errors.Is(err, cliconfig.ErrClusterNotFound) {
			return fmt.Errorf("cluster %q not in config", name)
		}
		return err
	}
	writeClusterDetail(cmd, entry, cfg.CurrentCluster == name, showToken)
	return nil
}

func writeClusterDetail(cmd *cobra.Command, entry *cliconfig.Cluster, current, showToken bool) {
	token := maskToken(entry.Token)
	if showToken {
		token = entry.Token
	}
	out := cmd.OutOrStdout()
	printf(out, "name: %s\n", entry.Name)
	printf(out, "server: %s\n", entry.Server)
	printf(out, "token: %s\n", token)
	if entry.TokenID != "" {
		printf(out, "token-id: %s\n", entry.TokenID)
	}
	printf(out, "current: %t\n", current)
}
