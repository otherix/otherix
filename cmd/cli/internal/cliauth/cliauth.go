// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package cliauth wires a cobra command's persistent flags into a
// fully-constructed authenticated cpclient.Client. It is the single
// source of truth for the precedence chain that turns flags + env
// + config file into a (endpoint, token) pair — every resource
// subcommand (vm, pool, node, ...) calls
// BuildClient instead of inlining its own flag plumbing.
//
// The package owns no state and has no test of its own: cliconfig
// covers the precedence logic, and vm subcommand tests exercise the
// composite path. cliauth's value is purely deduplication.
package cliauth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cliconfig"
	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

// Flag names shared with the root cobra command. Exported so future
// subcommands can read them via cmd.Flag(cliauth.FlagX) instead of
// re-hard-coding strings and risking drift.
const (
	FlagEndpoint = "endpoint"
	FlagToken    = "token"
	FlagCluster  = "cluster"
	FlagConfig   = "config"
)

// ResolveAuth inspects cmd's persistent flags + the process env and
// resolves the (endpoint, token, TLS-trust) tuple via cliconfig.Resolve,
// applying the documented flag → env → config precedence. The error
// chain is already operator-shaped (see translateResolveError); callers
// should `return err` from RunE. BuildClient layers a *cpclient.Client
// on top of this; the ssh connector consumes the raw tuple to build its
// own sshconn.Config, so the resolution lives here once.
func ResolveAuth(cmd *cobra.Command) (cliconfig.ResolvedAuth, error) {
	flagEndpoint, _ := cmd.Flags().GetString(FlagEndpoint)
	flagToken, _ := cmd.Flags().GetString(FlagToken)
	flagCluster, _ := cmd.Flags().GetString(FlagCluster)
	flagConfigPath, _ := cmd.Flags().GetString(FlagConfig)

	path, err := cliconfig.ResolvePath(flagConfigPath)
	if err != nil && !errors.Is(err, cliconfig.ErrNoHome) {
		return cliconfig.ResolvedAuth{}, err
	}
	var cfg *cliconfig.Config
	if path != "" {
		loaded, loadErr := cliconfig.Load(path)
		if loadErr != nil {
			return cliconfig.ResolvedAuth{}, fmt.Errorf("load config %s: %v", path, loadErr)
		}
		cfg = loaded
	}

	auth, err := cliconfig.Resolve(cliconfig.ResolveOptions{
		FlagEndpoint: flagEndpoint,
		FlagToken:    flagToken,
		FlagCluster:  flagCluster,
		EnvEndpoint:  os.Getenv(cliconfig.EnvServer),
		EnvToken:     os.Getenv(cliconfig.EnvToken),
		Config:       cfg,
	})
	if err != nil {
		return cliconfig.ResolvedAuth{}, translateResolveError(err, path)
	}
	return auth, nil
}

// BuildClient inspects cmd's persistent flags + the process env,
// resolves an (endpoint, token) pair via ResolveAuth, and returns a
// ready-to-use *cpclient.Client. The error chain surfaces actionable
// hints for each missing-credential case — callers should `return err`
// from RunE and let main render it.
func BuildClient(cmd *cobra.Command) (*cpclient.Client, error) {
	auth, err := ResolveAuth(cmd)
	if err != nil {
		return nil, err
	}

	var caPEM []byte
	if auth.CACertData != "" {
		decoded, derr := base64.StdEncoding.DecodeString(auth.CACertData)
		if derr != nil {
			return nil, fmt.Errorf("decode certificate-authority-data for cluster %q: %v", auth.ClusterName, derr)
		}
		caPEM = decoded
	}
	return cpclient.New(cpclient.Options{
		BaseURL:            auth.Endpoint,
		Token:              auth.Token,
		CACertPEM:          caPEM,
		InsecureSkipVerify: auth.InsecureSkipTLSVerify,
	})
}

// translateResolveError rewrites cliconfig sentinels into
// operator-facing messages with suggested actions. Keeps cliconfig's
// API surface lean (it knows about flags / env only in names,
// not commands) while still steering users to the right
// remediation in the CLI.
func translateResolveError(err error, path string) error {
	switch {
	case errors.Is(err, cliconfig.ErrEndpointMissing):
		return fmt.Errorf("no endpoint configured: pass --%s, set %s, or `otherix config use <cluster>`",
			FlagEndpoint, cliconfig.EnvServer)
	case errors.Is(err, cliconfig.ErrTokenMissing):
		return fmt.Errorf("no token configured: pass --%s, set %s, or `otherix config use <cluster>`",
			FlagToken, cliconfig.EnvToken)
	case errors.Is(err, cliconfig.ErrClusterNotFound):
		hint := ""
		if path != "" {
			hint = fmt.Sprintf(" (config: %s)", path)
		}
		return fmt.Errorf("%v%s — run `otherix config list` to see available clusters", err, hint)
	default:
		return err
	}
}
