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
