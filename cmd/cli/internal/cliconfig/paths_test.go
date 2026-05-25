// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package cliconfig_test

import (
	"path/filepath"
	"testing"

	"github.com/otherix/otherix/cmd/cli/internal/cliconfig"
)

func TestResolvePath_FlagWins(t *testing.T) {
	t.Setenv(cliconfig.ConfigEnvVar, "/env/path")
	t.Setenv("HOME", "/h")
	got, err := cliconfig.ResolvePath("/flag/path")
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if got != "/flag/path" {
		t.Errorf("flag precedence: got %q, want %q", got, "/flag/path")
	}
}

func TestResolvePath_EnvBeatsHome(t *testing.T) {
	t.Setenv(cliconfig.ConfigEnvVar, "/env/path")
	t.Setenv("HOME", "/h")
	got, err := cliconfig.ResolvePath("")
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if got != "/env/path" {
		t.Errorf("env precedence: got %q, want %q", got, "/env/path")
	}
}

func TestResolvePath_HomeFallback(t *testing.T) {
	t.Setenv(cliconfig.ConfigEnvVar, "")
	t.Setenv("HOME", "/h")
	got, err := cliconfig.ResolvePath("")
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	want := filepath.Join("/h", cliconfig.HomeRelativePath)
	if got != want {
		t.Errorf("home fallback: got %q, want %q", got, want)
	}
}

func TestResolvePath_EmptyEnvFallsThrough(t *testing.T) {
	// Setting the env var to an empty string must behave as unset —
	// kubectl and docker treat empty the same way, and we follow.
	t.Setenv(cliconfig.ConfigEnvVar, "")
	t.Setenv("HOME", "/h")
	got, err := cliconfig.ResolvePath("")
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	want := filepath.Join("/h", cliconfig.HomeRelativePath)
	if got != want {
		t.Errorf("empty env: got %q, want %q", got, want)
	}
}
