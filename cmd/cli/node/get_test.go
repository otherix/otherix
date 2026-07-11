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

func renderNodeText(t *testing.T, n cpclient.Node) string {
	t.Helper()
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printNodeText(cmd, n, false)
	return buf.String()
}

func lineFor(out, key string) (string, bool) {
	for ln := range strings.SplitSeq(out, "\n") {
		if v, ok := strings.CutPrefix(ln, key+": "); ok {
			return v, true
		}
	}
	return "", false
}

func TestPrintNodeTextCompressedSwap(t *testing.T) {
	n := cpclient.Node{
		Name:         "node-1",
		Capabilities: []byte(`{"kvm_available":true,"compressed_swap":{"kind":"zram","size_mib":768,"mem_limit_mib":256,"algorithm":"zstd","swapped_mib":100,"ram_used_mib":30}}`),
	}
	out := renderNodeText(t, n)
	want := "zram 768MiB (cap 256MiB) zstd  swapped 100MiB (13%)  ram 30MiB"
	if got, ok := lineFor(out, "compressed_swap"); !ok || got != want {
		t.Errorf("compressed_swap line = %q (present=%v), want %q", got, ok, want)
	}
}

func TestPrintNodeTextCompressedSwapInUse(t *testing.T) {
	n := cpclient.Node{
		Name:         "node-1",
		Capabilities: []byte(`{"compressed_swap":{"kind":"zram","size_mib":1980,"mem_limit_mib":0,"algorithm":"zstd","swapped_mib":240,"ram_used_mib":60}}`),
	}
	out := renderNodeText(t, n)
	want := "zram 1980MiB zstd  swapped 240MiB (12%)  ram 60MiB"
	if got, ok := lineFor(out, "compressed_swap"); !ok || got != want {
		t.Errorf("compressed_swap line = %q (present=%v), want %q", got, ok, want)
	}
}

func TestPrintNodeTextCompressedSwapNoCapWhenMemLimitZero(t *testing.T) {
	n := cpclient.Node{
		Name:         "node-1",
		Capabilities: []byte(`{"compressed_swap":{"kind":"zram","size_mib":768,"mem_limit_mib":0,"algorithm":"zstd"}}`),
	}
	out := renderNodeText(t, n)
	want := "zram 768MiB zstd  swapped 0MiB (0%)  ram 0MiB"
	got, ok := lineFor(out, "compressed_swap")
	if !ok || got != want {
		t.Errorf("compressed_swap line = %q (present=%v), want %q", got, ok, want)
	}
}

func TestPrintNodeTextCompressedSwapOff(t *testing.T) {
	n := cpclient.Node{Name: "node-1", Capabilities: []byte(`{"kvm_available":true}`)}
	out := renderNodeText(t, n)
	if got, ok := lineFor(out, "compressed_swap"); !ok || got != "off" {
		t.Errorf("compressed_swap line = %q (present=%v), want %q", got, ok, "off")
	}
}

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

// TestPrintNodeTextRoles verifies node get prints a roles: line joining the
// node's roles, and omits it when the wire envelope carried no roles.
func TestPrintNodeTextRoles(t *testing.T) {
	t.Run("gateway roles line", func(t *testing.T) {
		n := cpclient.Node{
			Name: "gw-1", Architecture: "arm64", Status: "ready", CreatedAt: "t0",
			Roles: []string{"gateway"},
		}
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		printNodeText(cmd, n, false)
		if !strings.Contains(buf.String(), "roles: gateway\n") {
			t.Errorf("printNodeText output missing %q\n%s", "roles: gateway", buf.String())
		}
	})

	t.Run("no roles omits line", func(t *testing.T) {
		n := cpclient.Node{Name: "n-1", Architecture: "arm64", Status: "ready", CreatedAt: "t0"}
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		printNodeText(cmd, n, false)
		if strings.Contains(buf.String(), "roles:") {
			t.Errorf("printNodeText rendered a roles line for a node without roles:\n%s", buf.String())
		}
	})
}

func TestPrintNodePressureWireguard(t *testing.T) {
	boom := "otwg0 ensure failed"
	cases := []struct {
		name string
		wg   *cpclient.NodeWireguard
		want string
	}{
		{name: "ready", wg: &cpclient.NodeWireguard{Status: "ready"}, want: "  wireguard: ok\n"},
		{name: "empty", wg: &cpclient.NodeWireguard{}, want: "  wireguard: ok\n"},
		{name: "failed", wg: &cpclient.NodeWireguard{Status: "failed", Error: &boom}, want: "  wireguard: failed (otwg0 ensure failed)\n"},
		{name: "pending", wg: &cpclient.NodeWireguard{Status: "pending"}, want: "  wireguard: pending\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := cpclient.Node{Name: "node-1", Architecture: "arm64", Status: "ready", CreatedAt: "t0", WireGuard: tc.wg}
			cmd := &cobra.Command{}
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			printNodeText(cmd, n, false)
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("printNodeText output missing %q\n%s", tc.want, buf.String())
			}
		})
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
