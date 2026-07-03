// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import "testing"

func TestNodeGatewayCommandShape(t *testing.T) {
	gw := newGatewayCommand()
	if gw.Use != "gateway" {
		t.Errorf("Use = %q, want %q", gw.Use, "gateway")
	}
	names := map[string]bool{}
	for _, c := range gw.Commands() {
		names[c.Name()] = true
	}
	if !names["enable"] || !names["disable"] {
		t.Errorf("gateway subcommands = %v, want enable and disable", names)
	}

	for _, c := range gw.Commands() {
		if c.Args == nil {
			t.Errorf("subcommand %q has nil Args, want cobra.ExactArgs(1)", c.Name())
			continue
		}
		if err := c.Args(c, []string{"n1"}); err != nil {
			t.Errorf("subcommand %q rejects one arg: %v", c.Name(), err)
		}
		if err := c.Args(c, []string{"n1", "n2"}); err == nil {
			t.Errorf("subcommand %q accepts two args, want ExactArgs(1)", c.Name())
		}
	}
}
