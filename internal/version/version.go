// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package version exposes build-time identification injected via -ldflags.
package version

// Build-time identification. Default values mark a non-release build; the
// Makefile and Dockerfiles override these via -ldflags.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// Info bundles the build-time identification of a binary.
type Info struct {
	Version string
	Commit  string
	Date    string
}

// Current returns the build-time identification of the running binary.
func Current() Info {
	return Info{Version: Version, Commit: Commit, Date: Date}
}
