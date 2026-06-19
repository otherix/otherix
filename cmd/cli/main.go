// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Command otherix is the operator-facing CLI for the Otherix control
// plane and its agents. It exposes resource subcommands (vm, pool, node,
// ...) plus ping-agent, which validates CP->agent mTLS reachability
// against a running otherix-agent.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
