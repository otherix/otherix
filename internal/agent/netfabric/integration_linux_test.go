// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

//go:build linux && integration
// +build linux,integration

package netfabric

import (
	"runtime"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// withNetNS runs fn inside a fresh, throwaway network namespace so the
// test never touches the host's real interfaces. It locks the goroutine
// to its OS thread for the duration, as required by the netns API. The
// test is skipped when the namespace cannot be created (typically a lack
// of CAP_NET_ADMIN); on Lima with root it runs for real.
func withNetNS(t *testing.T, fn func()) {
	t.Helper()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	orig, err := netns.Get()
	if err != nil {
		t.Fatalf("netns.Get() = %v", err)
	}
	defer func() {
		if err := netns.Set(orig); err != nil {
			t.Errorf("restore netns: %v", err)
		}
		if err := orig.Close(); err != nil {
			t.Errorf("close orig netns: %v", err)
		}
	}()

	ns, err := netns.New()
	if err != nil {
		t.Skipf("create netns (needs CAP_NET_ADMIN): %v", err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			t.Errorf("close test netns: %v", err)
		}
	}()

	fn()
}

func TestLinuxFabricBridgeLifecycle(t *testing.T) {
	withNetNS(t, func() {
		f := New()
		const name = "ot-br-test0"

		exists, err := f.BridgeExists(name)
		if err != nil {
			t.Fatalf("BridgeExists(%q) = %v", name, err)
		}
		if exists {
			t.Fatalf("BridgeExists(%q) = true before create, want false", name)
		}

		if err := f.EnsureBridge(name, 1450); err != nil {
			t.Fatalf("EnsureBridge(%q, 1450) = %v", name, err)
		}

		// Idempotent: a second EnsureBridge must succeed too.
		if err := f.EnsureBridge(name, 1450); err != nil {
			t.Fatalf("EnsureBridge(%q) second call = %v", name, err)
		}

		exists, err = f.BridgeExists(name)
		if err != nil {
			t.Fatalf("BridgeExists(%q) = %v", name, err)
		}
		if !exists {
			t.Fatalf("BridgeExists(%q) = false after create, want true", name)
		}

		link, err := netlink.LinkByName(name)
		if err != nil {
			t.Fatalf("LinkByName(%q) = %v", name, err)
		}
		if mtu := link.Attrs().MTU; mtu != 1450 {
			t.Errorf("bridge MTU = %d, want 1450", mtu)
		}
		if link.Attrs().Flags&1 == 0 { // net.FlagUp == 1
			t.Errorf("bridge not up after EnsureBridge")
		}

		if err := f.RemoveBridge(name); err != nil {
			t.Fatalf("RemoveBridge(%q) = %v", name, err)
		}

		// Idempotent: removing an absent bridge is a no-op.
		if err := f.RemoveBridge(name); err != nil {
			t.Fatalf("RemoveBridge(%q) on absent = %v", name, err)
		}

		exists, err = f.BridgeExists(name)
		if err != nil {
			t.Fatalf("BridgeExists(%q) = %v", name, err)
		}
		if exists {
			t.Fatalf("BridgeExists(%q) = true after remove, want false", name)
		}
	})
}
