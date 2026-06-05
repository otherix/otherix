// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Command otherix is the operator-facing CLI for the Otherix control
// plane and its agents. Iteration 2 ships a single subcommand —
// ping-agent — used to validate CP→agent mTLS reachability against a
// running otherix-agent. Future iterations layer on resource
// subcommands (vm, pool, node, ...) without changing the binary name.
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
