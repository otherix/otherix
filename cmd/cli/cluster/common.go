// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cluster

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/clierr"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagOutput = "output"
	flagForce  = "force"
)

func clientFromFlags(cmd *cobra.Command) (*cpclient.Client, error) {
	return cliauth.BuildClient(cmd)
}

func printf(cmd *cobra.Command, format string, args ...any) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), format, args...)
}

func classifyError(err error) error {
	return clierr.Classify(err)
}

func outputFormat(cmd *cobra.Command, defaultFormat string) (string, error) {
	raw, err := cmd.Flags().GetString(flagOutput)
	if err != nil {
		return "", err
	}
	if raw == "" {
		raw = defaultFormat
	}
	switch raw {
	case "text", "json":
		return raw, nil
	}
	return "", fmt.Errorf("--%s: unknown format %q (text, json)", flagOutput, raw)
}

// stdinIsTerminal reports whether stdin is a TTY — used to gate
// interactive prompts. Pipes / heredocs / CI stdin all return false,
// in which case missing required confirmations must come from --force.
func stdinIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// confirmYN reads y/N from stdin, treating empty / non-y prefix as
// "no". Returns false when stdin is not a TTY — non-interactive
// contexts must use --force to pre-confirm. EOF on pipe-close also
// reads as "no" (read-error path).
func confirmYN(cmd *cobra.Command, prompt string) (bool, error) {
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "%s [y/N]: ", prompt); err != nil {
		return false, err
	}
	reader := bufio.NewReader(cmd.InOrStdin())
	line, _ := reader.ReadString('\n')
	trimmed := strings.TrimSpace(strings.ToLower(line))
	return trimmed == "y" || trimmed == "yes", nil
}
