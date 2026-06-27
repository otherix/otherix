// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package user

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"sigs.k8s.io/yaml"

	"github.com/otherix/otherix/cmd/cli/internal/cliauth"
	"github.com/otherix/otherix/cmd/cli/internal/clierr"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

const (
	flagOutput        = "output"
	flagLimit         = "limit"
	flagCursor        = "cursor"
	flagEmail         = "email"
	flagDisplayName   = "display-name"
	flagRole          = "role"
	flagPasswordStdin = "password-stdin"

	defaultListLimit = 20
)

func clientFromFlags(cmd *cobra.Command) (*cpclient.Client, error) {
	return cliauth.BuildClient(cmd)
}

func printf(cmd *cobra.Command, format string, args ...any) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), format, args...)
}

// printNextCursor prints a copy-pasteable next-page hint for a
// cursor-paginated table listing. No-op when there is no next page.
func printNextCursor(cmd *cobra.Command, next string) {
	if next == "" {
		return
	}
	printf(cmd, "\nMore results - next page (re-add any filters):\n  %s --cursor %s\n", cmd.CommandPath(), next)
}

// classifyError maps a cpclient error to a stable-prefixed operator
// message. Delegates to clierr.Classify (shared with the other CLI
// surfaces).
func classifyError(err error) error {
	return clierr.Classify(err)
}

// outputFormat reads the --output flag (default defaultFormat). The base
// formats text/json/table are always accepted; extra lists additional
// formats a command opts into (yaml, only on get/list). Unknown values
// surface as usage errors.
func outputFormat(cmd *cobra.Command, defaultFormat string, extra ...string) (string, error) {
	raw, err := cmd.Flags().GetString(flagOutput)
	if err != nil {
		return "", err
	}
	if raw == "" {
		raw = defaultFormat
	}
	allowed := append([]string{"text", "json", "table"}, extra...)
	if slices.Contains(allowed, raw) {
		return raw, nil
	}
	return "", fmt.Errorf("--%s: unknown format %q (%s)", flagOutput, raw, strings.Join(allowed, ", "))
}

// renderUser writes a single user in the requested format. json and yaml
// re-marshal the decoded view (the cpclient methods that return a User do
// not surface the raw server bytes), so absent optional fields are
// omitted via the User struct's omitempty tags. text prints the
// human-readable key/value block.
func renderUser(cmd *cobra.Command, u cpclient.User, format string) error {
	switch format {
	case "json":
		raw, err := json.MarshalIndent(u, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %v", err)
		}
		printf(cmd, "%s\n", raw)
	case "yaml":
		out, err := yaml.Marshal(u)
		if err != nil {
			return fmt.Errorf("marshal yaml: %v", err)
		}
		printf(cmd, "%s", out)
	default:
		printUserText(cmd, u)
	}
	return nil
}

func printUserText(cmd *cobra.Command, u cpclient.User) {
	printf(cmd, "username: %s\n", u.Username)
	printf(cmd, "role: %s\n", u.Role)
	if u.Email != "" {
		printf(cmd, "email: %s\n", u.Email)
	}
	if u.DisplayName != "" {
		printf(cmd, "display_name: %s\n", u.DisplayName)
	}
	printf(cmd, "id: %s\n", u.ID)
	printf(cmd, "created_at: %s\n", u.CreatedAt)
	printf(cmd, "updated_at: %s\n", u.UpdatedAt)
	if u.LastLoginAt != nil && *u.LastLoginAt != "" {
		printf(cmd, "last_login_at: %s\n", *u.LastLoginAt)
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// resolveUserID resolves a username to its UUID via the cpclient guard
// lookup, translating the package's not-found sentinel into a clean
// operator message so a missing user reads as "user not found" rather
// than an api_error envelope.
func resolveUserID(ctx context.Context, c *cpclient.Client, username string) (cpclient.User, error) {
	u, err := c.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, cpclient.ErrNotFound) {
			return cpclient.User{}, fmt.Errorf("user %q not found", username)
		}
		return cpclient.User{}, classifyError(err)
	}
	return u, nil
}

// readPassword obtains a password without ever placing it in argv. When
// --password-stdin is set it reads exactly one line from stdin (the
// automation path); otherwise it prompts interactively with terminal
// echo disabled. The returned password is rejected when empty so a stray
// blank line does not silently set an empty credential.
func readPassword(cmd *cobra.Command, prompt string) (string, error) {
	useStdin, err := cmd.Flags().GetBool(flagPasswordStdin)
	if err != nil {
		return "", err
	}
	if useStdin {
		return readPasswordLine(cmd.InOrStdin())
	}
	pw, err := promptPassword(cmd, prompt)
	if err != nil {
		return "", err
	}
	return pw, nil
}

// readPasswordLine reads a single line from r and strips the trailing
// newline. Used by the --password-stdin path so a piped secret never
// reaches the command line.
func readPasswordLine(r io.Reader) (string, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read password from stdin: %v", err)
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("empty password on stdin")
	}
	return line, nil
}

// promptPassword reads a password from the controlling terminal with
// echo disabled (term.ReadPassword over os.Stdin's fd). Not a TTY (e.g.
// a test or automation that did not pass --password-stdin) is an error
// telling the operator to use --password-stdin instead.
func promptPassword(cmd *cobra.Command, prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("no terminal for password prompt - pipe the password and pass --password-stdin")
	}
	printf(cmd, "%s", prompt)
	raw, err := term.ReadPassword(fd)
	printf(cmd, "\n")
	if err != nil {
		return "", fmt.Errorf("read password: %v", err)
	}
	pw := strings.TrimRight(string(raw), "\r\n")
	if pw == "" {
		return "", errors.New("empty password")
	}
	return pw, nil
}
