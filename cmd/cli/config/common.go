// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/cliconfig"
)

// promptInput reads a line of user input from stdin, trimming the
// trailing newline. Used for plain-text fields (cluster name,
// server URL, login). The label is printed to stderr so test
// fixtures that capture stdout for assertions are not polluted by
// prompts.
func promptInput(out io.Writer, label string, reader *bufio.Reader) (string, error) {
	if _, err := fmt.Fprintf(out, "%s: ", label); err != nil {
		return "", err
	}
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// promptPassword reads a password from /dev/stdin (or whatever
// terminal stdin is bound to) without echoing the typed characters.
// Uses golang.org/x/term.ReadPassword which falls back to a
// raw-mode read on the underlying file descriptor.
func promptPassword(out io.Writer, label string) (string, error) {
	if _, err := fmt.Fprintf(out, "%s: ", label); err != nil {
		return "", err
	}
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	if _, ferr := fmt.Fprintln(out); ferr != nil && err == nil {
		err = ferr
	}
	if err != nil {
		return "", fmt.Errorf("read password: %v", err)
	}
	return string(raw), nil
}

// stdinIsTerminal reports whether stdin is a TTY — used to gate
// interactive prompts. Pipes / heredocs / CI stdin all return
// false, in which case missing required values must come from
// flags or env.
func stdinIsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// resolveConfigPath sits between cobra and cliconfig for the
// `config` subcommands: each of add / list / use / remove / show
// inherits --config from the root, and every one needs the same
// "use the flag if set, else default" lookup.
func resolveConfigPath(cmd *cobra.Command) (string, error) {
	flag, _ := cmd.Flags().GetString(cliauth.FlagConfig)
	path, err := cliconfig.ResolvePath(flag)
	if err != nil {
		return "", err
	}
	return path, nil
}

// maskToken returns a display-safe rendering of a plaintext API
// token. The first 8 chars ("otx_xxxx") survive — they match the
// stored Prefix and suffice to distinguish two tokens — a the rest
// is replaced with `***`. Tokens shorter than 8 chars (corrupt
// inputs) fall back to a pure `***`.
func maskToken(token string) string {
	const visible = 8
	if len(token) <= visible {
		return "***"
	}
	return token[:visible] + "***"
}

// confirmYN reads y/N from stdin, treating empty (just Enter) as
// "no". Returns false when stdin is not a TTY and noTTYDefault is
// false — non-interactive contexts must use --force to pre-confirm.
func confirmYN(out io.Writer, reader *bufio.Reader, prompt string) (bool, error) {
	if _, err := fmt.Fprintf(out, "%s [y/N]: ", prompt); err != nil {
		return false, err
	}
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	trimmed := strings.TrimSpace(strings.ToLower(line))
	return trimmed == "y" || trimmed == "yes", nil
}
