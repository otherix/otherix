// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/otherix/otherix/cmd/cli/internal/cpclient"
)

func renderVMText(t *testing.T, vm cpclient.VM) string {
	t.Helper()
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	printVMText(cmd, vm)
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

// TestPrintVMTextOwner covers the owner rendering: when the server
// resolved an owner display_name (caller holds user:read) the text form
// shows `owner:` and suppresses the raw `owner_id:` line; otherwise it
// falls back to `owner_id:` with the UUID.
func TestPrintVMTextOwner(t *testing.T) {
	owner := "Ada Lovelace"
	t.Run("resolved owner shows name, hides owner_id", func(t *testing.T) {
		out := renderVMText(t, cpclient.VM{
			OwnerID: "71c47f04-9e3b-40e5-9050-fa12a13e208e", Owner: &owner,
		})
		if got, ok := lineFor(out, "owner"); !ok || got != owner {
			t.Errorf("owner line = %q (present=%v), want %q", got, ok, owner)
		}
		if _, ok := lineFor(out, "owner_id"); ok {
			t.Errorf("owner_id line present, want suppressed when owner resolved")
		}
	})

	t.Run("unresolved owner falls back to owner_id", func(t *testing.T) {
		out := renderVMText(t, cpclient.VM{
			OwnerID: "71c47f04-9e3b-40e5-9050-fa12a13e208e", Owner: nil,
		})
		if got, ok := lineFor(out, "owner_id"); !ok || got != "71c47f04-9e3b-40e5-9050-fa12a13e208e" {
			t.Errorf("owner_id line = %q (present=%v), want the UUID", got, ok)
		}
		if _, ok := lineFor(out, "owner"); ok {
			t.Errorf("owner line present, want absent when owner unresolved")
		}
	})
}

// TestPrintVMTextStatus covers the status line rendering off the nested
// status object: a settled phase prints bare, while a pending VM appends
// the scheduling reason/message so operators see why it is unbound.
func TestPrintVMTextStatus(t *testing.T) {
	cases := []struct {
		name   string
		status cpclient.VMStatus
		want   string
	}{
		{"running bare", cpclient.VMStatus{Phase: "running"}, "running"},
		{
			"pending reason+message",
			cpclient.VMStatus{Phase: "pending", Reason: "pool_not_ready", Message: `pool "default" not ready`},
			`pending (pool_not_ready: pool "default" not ready)`,
		},
		{"pending reason only", cpclient.VMStatus{Phase: "pending", Reason: "pending_schedule"}, "pending (pending_schedule)"},
		{"pending no reason", cpclient.VMStatus{Phase: "pending"}, "pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := renderVMText(t, cpclient.VM{Status: tc.status})
			if got, ok := lineFor(out, "status"); !ok || got != tc.want {
				t.Errorf("status line = %q (present=%v), want %q", got, ok, tc.want)
			}
		})
	}
}

// TestPrintVMTextCloudInit covers the presence indicators for the
// write-once-read cloud-init payloads: when set, the text view shows a
// "<set, N bytes>" indicator (not the blob, which can be large - operators
// reach for -o yaml / -o json to see the content); when unset, the line is
// omitted entirely.
func TestPrintVMTextCloudInit(t *testing.T) {
	userData := "#cloud-config\nhostname: h\n"
	netCfg := "version: 2\n"
	t.Run("present shows byte count, not content", func(t *testing.T) {
		out := renderVMText(t, cpclient.VM{UserData: &userData, NetworkConfig: &netCfg})
		if got, ok := lineFor(out, "user_data"); !ok || got != "<set, 26 bytes>" {
			t.Errorf("user_data line = %q (present=%v), want %q", got, ok, "<set, 26 bytes>")
		}
		if got, ok := lineFor(out, "network_config"); !ok || got != "<set, 11 bytes>" {
			t.Errorf("network_config line = %q (present=%v), want %q", got, ok, "<set, 11 bytes>")
		}
		if strings.Contains(out, "#cloud-config") || strings.Contains(out, "version: 2") {
			t.Errorf("text view leaked cloud-init content:\n%s", out)
		}
	})
	t.Run("unset omits both lines", func(t *testing.T) {
		out := renderVMText(t, cpclient.VM{})
		if _, ok := lineFor(out, "user_data"); ok {
			t.Error("user_data line present for nil payload, want omitted")
		}
		if _, ok := lineFor(out, "network_config"); ok {
			t.Error("network_config line present for nil payload, want omitted")
		}
	})
}

// TestPrintVMTextMemoryUsed covers the observed guest memory-used line: the
// value when the guest balloon has reported, and <unset> when nil (VM not
// running or the guest has not reported yet). The line is always present so
// the observed field is discoverable in the text view.
func TestPrintVMTextMemoryUsed(t *testing.T) {
	used := int64(206)
	t.Run("reported value", func(t *testing.T) {
		out := renderVMText(t, cpclient.VM{Status: cpclient.VMStatus{Phase: "running", MemoryUsedMib: &used}})
		if got, ok := lineFor(out, "memory_used_mib"); !ok || got != "206" {
			t.Errorf("memory_used_mib line = %q (present=%v), want %q", got, ok, "206")
		}
	})
	t.Run("nil renders unset", func(t *testing.T) {
		out := renderVMText(t, cpclient.VM{Status: cpclient.VMStatus{Phase: "stopped"}})
		if got, ok := lineFor(out, "memory_used_mib"); !ok || got != "<unset>" {
			t.Errorf("memory_used_mib line = %q (present=%v), want %q", got, ok, "<unset>")
		}
	})
}

// TestPrintVMTextImageSHA256 covers the conditional image_sha256 line:
// shown when the VM row carries a digest, omitted when empty.
func TestPrintVMTextImageSHA256(t *testing.T) {
	sha := "abcd1234"
	if got, ok := lineFor(renderVMText(t, cpclient.VM{ImageSHA256: sha}), "image_sha256"); !ok || got != sha {
		t.Errorf("image_sha256 line = %q (present=%v), want %q", got, ok, sha)
	}
	if _, ok := lineFor(renderVMText(t, cpclient.VM{ImageSHA256: ""}), "image_sha256"); ok {
		t.Error("image_sha256 line present for empty digest, want omitted")
	}
}
