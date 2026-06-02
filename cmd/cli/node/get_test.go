// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package node

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func TestPrintNodeWireguard(t *testing.T) {
	name := "node-2"
	n := cpclient.Node{
		Name: "node-1", Architecture: "arm64", Status: "ready", CreatedAt: "t0",
		WireGuard: &cpclient.NodeWireguard{
			OverlayIP: "10.42.0.1/16", PublicKey: "pub==", ListenPort: 51820,
			Endpoint: "192.168.104.2:51820",
			Peers: []cpclient.NodeWireguardPeer{
				{NodeID: "id-b", NodeName: &name, OverlayIP: "10.42.0.2", Established: true},
			},
		},
	}
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printNodeText(cmd, n, false)
	out := buf.String()
	for _, want := range []string{"overlay_ip: 10.42.0.1/16", "node-2", "established"} {
		if !strings.Contains(out, want) {
			t.Errorf("printNodeText output missing %q\n%s", want, out)
		}
	}
}

func TestPrintNodeWireguardAbsent(t *testing.T) {
	n := cpclient.Node{Name: "node-1", Architecture: "arm64", Status: "ready", CreatedAt: "t0"}
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printNodeText(cmd, n, false)
	if strings.Contains(buf.String(), "wireguard") {
		t.Errorf("printNodeText rendered a wireguard section for a node without one:\n%s", buf.String())
	}
}
