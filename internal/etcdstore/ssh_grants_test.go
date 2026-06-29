//go:build integration
// +build integration

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore_test

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

func TestSSHGrant_CreateLookupMutateRevoke(t *testing.T) {
	st, _ := startStore(t)
	ctx := context.Background()
	hash := auth.HashToken("otx_sshgrant_example")

	g, err := st.CreateSSHGrant(ctx, store.CreateSSHGrantParams{
		Name: "acme-alice", CreatedBy: uuid.New(), RecipientLabel: "alice",
		TokenHash: hash, VMs: []store.SSHGrantVM{{VMName: "web01", Login: "dev"}},
	})
	if err != nil {
		t.Fatalf("CreateSSHGrant() error = %v", err)
	}
	// Duplicate name rejected.
	if _, err := st.CreateSSHGrant(ctx, store.CreateSSHGrantParams{
		Name: "acme-alice", CreatedBy: uuid.New(), TokenHash: auth.HashToken("x"),
	}); err == nil {
		t.Errorf("CreateSSHGrant duplicate name = nil error, want conflict")
	}
	// Lookup by token hash.
	byTok, err := st.SSHGrantByTokenHash(ctx, hash)
	if err != nil || byTok.ID != g.ID {
		t.Fatalf("SSHGrantByTokenHash = (%v,%v), want (%v,nil)", byTok.ID, err, g.ID)
	}
	// Lookup by id and name resolve the same grant.
	if byID, err := st.SSHGrantByID(ctx, g.ID); err != nil || byID.ID != g.ID {
		t.Fatalf("SSHGrantByID = (%v,%v), want (%v,nil)", byID.ID, err, g.ID)
	}
	if byName, err := st.SSHGrantByName(ctx, "ACME-ALICE"); err != nil || byName.ID != g.ID {
		t.Fatalf("SSHGrantByName(case-insensitive) = (%v,%v), want (%v,nil)", byName.ID, err, g.ID)
	}
	// Add a second VM; scope grows.
	upd, err := st.AddSSHGrantVM(ctx, g.ID, store.SSHGrantVM{VMName: "web02", Login: "ubuntu"})
	if err != nil {
		t.Fatalf("AddSSHGrantVM() error = %v", err)
	}
	if got := loginFor(upd.VMs, "web02"); got != "ubuntu" {
		t.Errorf("web02 login = %q, want ubuntu", got)
	}
	// Re-adding an existing VM replaces the login (idempotent on vm_name).
	upd, err = st.AddSSHGrantVM(ctx, g.ID, store.SSHGrantVM{VMName: "web02", Login: "admin"})
	if err != nil {
		t.Fatalf("AddSSHGrantVM(replace) error = %v", err)
	}
	if got := loginFor(upd.VMs, "web02"); got != "admin" {
		t.Errorf("web02 login after replace = %q, want admin", got)
	}
	if n := countVM(upd.VMs, "web02"); n != 1 {
		t.Errorf("web02 entry count = %d, want 1 (no duplicate)", n)
	}
	// Remove web01.
	upd, err = st.RemoveSSHGrantVM(ctx, g.ID, "web01")
	if err != nil {
		t.Fatalf("RemoveSSHGrantVM() error = %v", err)
	}
	if loginFor(upd.VMs, "web01") != "" {
		t.Errorf("web01 still present after removal: %v", upd.VMs)
	}
	// Revoke; row survives but flagged.
	if err := st.RevokeSSHGrant(ctx, g.ID); err != nil {
		t.Fatalf("RevokeSSHGrant() error = %v", err)
	}
	after, err := st.SSHGrantByTokenHash(ctx, hash)
	if err != nil {
		t.Fatalf("SSHGrantByTokenHash after revoke error = %v", err)
	}
	if !after.Revoked {
		t.Errorf("grant.Revoked = false after RevokeSSHGrant, want true")
	}
}

// TestSSHGrant_CreateClearsKeysAtomically asserts the create commits the row,
// the name-uniqueness guard, and the token-hash index together, and the delete
// clears all three - no torn state (a token index pointing at a missing row, or
// a row unreachable by its token).
func TestSSHGrant_CreateClearsKeysAtomically(t *testing.T) {
	st, c := startStore(t)
	ctx := context.Background()
	hash := auth.HashToken("otx_sshgrant_atomic")

	g, err := st.CreateSSHGrant(ctx, store.CreateSSHGrantParams{
		Name: "Atomic-Grant", CreatedBy: uuid.New(), RecipientLabel: "bob",
		TokenHash: hash, VMs: []store.SSHGrantVM{{VMName: "db01", Login: "ops"}},
	})
	if err != nil {
		t.Fatalf("CreateSSHGrant() error = %v", err)
	}

	rowKey := etcd.Key("ssh_grants", g.ID.String())
	nameKey := etcd.Key("uniq", "ssh_grants", "name", strings.ToLower("Atomic-Grant"))
	tokenKey := etcd.Key("idx", "ssh_grants", "token", hex.EncodeToString(hash))

	for _, k := range []string{rowKey, nameKey, tokenKey} {
		if _, found, err := c.Get(ctx, k); err != nil || !found {
			t.Fatalf("after create, key %q present = %v (err %v), want true", k, found, err)
		}
	}

	if err := st.DeleteSSHGrant(ctx, g.ID); err != nil {
		t.Fatalf("DeleteSSHGrant() error = %v", err)
	}
	for _, k := range []string{rowKey, nameKey, tokenKey} {
		if _, found, err := c.Get(ctx, k); err != nil || found {
			t.Errorf("after delete, key %q present = %v (err %v), want false", k, found, err)
		}
	}
	// Lookups now miss.
	if _, err := st.SSHGrantByID(ctx, g.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SSHGrantByID after delete = %v, want ErrNotFound", err)
	}
	if _, err := st.SSHGrantByTokenHash(ctx, hash); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SSHGrantByTokenHash after delete = %v, want ErrNotFound", err)
	}
}

func TestSSHGrant_List(t *testing.T) {
	st, _ := startStore(t)
	ctx := context.Background()

	for _, name := range []string{"grant-a", "grant-b", "grant-c"} {
		if _, err := st.CreateSSHGrant(ctx, store.CreateSSHGrantParams{
			Name: name, CreatedBy: uuid.New(), TokenHash: auth.HashToken("tok-" + name),
		}); err != nil {
			t.Fatalf("CreateSSHGrant(%q) error = %v", name, err)
		}
	}
	all, err := st.ListSSHGrants(ctx, store.ListSSHGrantsParams{LimitCount: 50})
	if err != nil {
		t.Fatalf("ListSSHGrants() error = %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListSSHGrants() returned %d grants, want 3", len(all))
	}
	// Limit caps the page.
	page, err := st.ListSSHGrants(ctx, store.ListSSHGrantsParams{LimitCount: 2})
	if err != nil {
		t.Fatalf("ListSSHGrants(limit=2) error = %v", err)
	}
	if len(page) != 2 {
		t.Errorf("ListSSHGrants(limit=2) returned %d grants, want 2", len(page))
	}
}

func loginFor(vms []store.SSHGrantVM, name string) string {
	for _, v := range vms {
		if v.VMName == name {
			return v.Login
		}
	}
	return ""
}

func countVM(vms []store.SSHGrantVM, name string) int {
	n := 0
	for _, v := range vms {
		if v.VMName == name {
			n++
		}
	}
	return n
}
