// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package bootstrap

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestResolveServiceOwner(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "certs")
	if err := os.Mkdir(child, 0o750); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}

	uid, gid, ok, err := resolveServiceOwner(child)
	if err != nil {
		t.Fatalf("resolveServiceOwner(%q) error = %v", child, err)
	}

	if os.Getuid() == 0 {
		// Running as root: t.TempDir() is root-owned, so there is no separate
		// service user to hand off to.
		if ok {
			t.Errorf("resolveServiceOwner ok = true, want false when the parent is root-owned")
		}
		return
	}
	// Non-root test process: the temp tree is owned by us, so the parent's
	// owner is our own uid/gid and material should be handed to it.
	wantUID, wantGID := os.Getuid(), os.Getgid()
	if !ok || uid != wantUID || gid != wantGID {
		t.Errorf("resolveServiceOwner = (%d, %d, %v), want (%d, %d, true)", uid, gid, ok, wantUID, wantGID)
	}
}

func TestHandToServiceUserNonRootIsNoop(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this test asserts the non-root no-op; running as root")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "agent.key")
	if err := os.WriteFile(f, []byte("k"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	before, err := os.Stat(f)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	if err := handToServiceUser(f); err != nil {
		t.Fatalf("handToServiceUser (non-root) = %v, want nil", err)
	}

	after, err := os.Stat(f)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	bs, _ := before.Sys().(*syscall.Stat_t)
	as, _ := after.Sys().(*syscall.Stat_t)
	if bs == nil || as == nil {
		t.Skip("non-unix stat; ownership assertion not applicable")
	}
	if bs.Uid != as.Uid || bs.Gid != as.Gid {
		t.Errorf("ownership changed as non-root: was %d:%d, now %d:%d", bs.Uid, bs.Gid, as.Uid, as.Gid)
	}
}

func TestPersistWritesAllThreeFiles(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "agent.key")
	certPath := filepath.Join(dir, "agent.crt")
	caPath := filepath.Join(dir, "ca.crt")

	res := &Result{KeyPEM: []byte("key"), CertPEM: []byte("cert"), CACertPEM: []byte("ca")}
	if err := Persist(certPath, keyPath, caPath, res); err != nil {
		t.Fatalf("Persist() error = %v", err)
	}
	for _, p := range []string{keyPath, certPath, caPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to exist: %v", p, err)
		}
	}
}
