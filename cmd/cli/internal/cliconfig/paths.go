// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cliconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ConfigEnvVar names the env override for the config file path —
// mirrors KUBECONFIG / DOCKER_CONFIG. Empty или unset → fall through
// to the home-directory default.
const ConfigEnvVar = "OTHERIX_CONFIG"

// HomeRelativePath is the path inside the user's home directory that
// holds the default config. Lives at the same level as ~/.kube/ /
// ~/.docker/ for muscle-memory parity.
const HomeRelativePath = ".otherix/config"

// ErrNoHome is returned by ResolvePath when both $OTHERIX_CONFIG and
// $HOME are missing — a degenerate environment where the CLI cannot
// pick a default file location. Callers translate this к "set
// OTHERIX_CONFIG or pass --config".
var ErrNoHome = errors.New("cliconfig: no $HOME and no OTHERIX_CONFIG — cannot resolve default config path")

// ResolvePath picks the config file path в this order:
//
//  1. flagPath (the --config flag) — caller's strongest intent;
//  2. $OTHERIX_CONFIG — operator override at the shell layer;
//  3. $HOME/.otherix/config — the default ("kubectl-style" location).
//
// XDG_CONFIG_HOME is deliberately not consulted. kubectl и docker
// both ship per-tool home-relative defaults (~/.kube, ~/.docker)
// и we follow the same pattern для predictability — users grep'ing
// "where is my config?" find one canonical answer.
//
// The path may not exist yet; cliconfig.Load tolerates absent files.
func ResolvePath(flagPath string) (string, error) {
	if flagPath != "" {
		return flagPath, nil
	}
	if v := os.Getenv(ConfigEnvVar); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("%w: %v", ErrNoHome, err)
	}
	return filepath.Join(home, HomeRelativePath), nil
}
