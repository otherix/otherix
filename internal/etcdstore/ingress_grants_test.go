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

	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

func TestIngressGrant_CreateLookupMutateRevoke(t *testing.T) {
	st, _ := startStore(t)
	ctx := context.Background()
	hash := auth.HashToken("otx_ingressgrant_example")

	sourceIP := "203.0.113.0/24"
	g, err := st.CreateIngressGrant(ctx, store.CreateIngressGrantParams{
		Name: "acme-alice", CreatedBy: uuid.New(), RecipientLabel: "alice",
		TokenHash: hash, VMs: []store.IngressGrantVM{{VMName: "web01", Ports: []int{22, 5432}, Login: "dev"}},
		SourceIP: &sourceIP,
	})
	if err != nil {
		t.Fatalf("CreateIngressGrant() error = %v", err)
	}
	if g.SourceIP == nil || *g.SourceIP != sourceIP {
		t.Errorf("CreateIngressGrant SourceIP = %v, want %q", g.SourceIP, sourceIP)
	}
	// Ports round-trip through the JSON persistence unchanged.
	if diff := cmp.Diff([]store.IngressGrantVM{{VMName: "web01", Ports: []int{22, 5432}, Login: "dev"}}, g.VMs); diff != "" {
		t.Errorf("CreateIngressGrant VMs mismatch (-want +got):\n%s", diff)
	}
	// Duplicate name rejected.
	if _, err := st.CreateIngressGrant(ctx, store.CreateIngressGrantParams{
		Name: "acme-alice", CreatedBy: uuid.New(), TokenHash: auth.HashToken("x"),
	}); err == nil {
		t.Errorf("CreateIngressGrant duplicate name = nil error, want conflict")
	}
	// Lookup by token hash.
	byTok, err := st.IngressGrantByTokenHash(ctx, hash)
	if err != nil || byTok.ID != g.ID {
		t.Fatalf("IngressGrantByTokenHash = (%v,%v), want (%v,nil)", byTok.ID, err, g.ID)
	}
	// Ports survive a real read-back (JSON deserialization from etcd).
	if diff := cmp.Diff([]store.IngressGrantVM{{VMName: "web01", Ports: []int{22, 5432}, Login: "dev"}}, byTok.VMs); diff != "" {
		t.Errorf("IngressGrantByTokenHash VMs mismatch (-want +got):\n%s", diff)
	}
	// The source-IP pin survives a real read-back.
	if byTok.SourceIP == nil || *byTok.SourceIP != sourceIP {
		t.Errorf("IngressGrantByTokenHash SourceIP = %v, want %q", byTok.SourceIP, sourceIP)
	}
	// Lookup by id and name resolve the same grant.
	if byID, err := st.IngressGrantByID(ctx, g.ID); err != nil || byID.ID != g.ID {
		t.Fatalf("IngressGrantByID = (%v,%v), want (%v,nil)", byID.ID, err, g.ID)
	}
	if byName, err := st.IngressGrantByName(ctx, "ACME-ALICE"); err != nil || byName.ID != g.ID {
		t.Fatalf("IngressGrantByName(case-insensitive) = (%v,%v), want (%v,nil)", byName.ID, err, g.ID)
	}
	// Add a second VM; scope grows. The VMID binds the grant to a stable id.
	web02a := uuid.New()
	upd, err := st.AddIngressGrantVM(ctx, g.ID, store.IngressGrantVM{VMName: "web02", VMID: web02a, Login: "ubuntu"})
	if err != nil {
		t.Fatalf("AddIngressGrantVM() error = %v", err)
	}
	if got := loginFor(upd.VMs, "web02"); got != "ubuntu" {
		t.Errorf("web02 login = %q, want ubuntu", got)
	}
	if got := vmIDFor(upd.VMs, "web02"); got != web02a {
		t.Errorf("web02 vm_id = %v, want %v", got, web02a)
	}
	// Re-adding an existing VM replaces the login AND re-binds the VMID
	// (idempotent on vm_name): a name re-added after the old VM was deleted and
	// reused must track the current VM's identity, not the stale one.
	web02b := uuid.New()
	upd, err = st.AddIngressGrantVM(ctx, g.ID, store.IngressGrantVM{VMName: "web02", VMID: web02b, Login: "admin"})
	if err != nil {
		t.Fatalf("AddIngressGrantVM(replace) error = %v", err)
	}
	if got := loginFor(upd.VMs, "web02"); got != "admin" {
		t.Errorf("web02 login after replace = %q, want admin", got)
	}
	if got := vmIDFor(upd.VMs, "web02"); got != web02b {
		t.Errorf("web02 vm_id after replace = %v, want %v (re-bound)", got, web02b)
	}
	if n := countVM(upd.VMs, "web02"); n != 1 {
		t.Errorf("web02 entry count = %d, want 1 (no duplicate)", n)
	}
	// Remove web01.
	upd, err = st.RemoveIngressGrantVM(ctx, g.ID, "web01")
	if err != nil {
		t.Fatalf("RemoveIngressGrantVM() error = %v", err)
	}
	if loginFor(upd.VMs, "web01") != "" {
		t.Errorf("web01 still present after removal: %v", upd.VMs)
	}
	// Revoke; row survives but flagged.
	if err := st.RevokeIngressGrant(ctx, g.ID); err != nil {
		t.Fatalf("RevokeIngressGrant() error = %v", err)
	}
	after, err := st.IngressGrantByTokenHash(ctx, hash)
	if err != nil {
		t.Fatalf("IngressGrantByTokenHash after revoke error = %v", err)
	}
	if !after.Revoked {
		t.Errorf("grant.Revoked = false after RevokeIngressGrant, want true")
	}
}

// TestIngressGrant_CreateClearsKeysAtomically asserts the create commits the row,
// the name-uniqueness guard, and the token-hash index together, and the delete
// clears all three - no torn state (a token index pointing at a missing row, or
// a row unreachable by its token).
func TestIngressGrant_CreateClearsKeysAtomically(t *testing.T) {
	st, c := startStore(t)
	ctx := context.Background()
	hash := auth.HashToken("otx_ingressgrant_atomic")

	g, err := st.CreateIngressGrant(ctx, store.CreateIngressGrantParams{
		Name: "Atomic-Grant", CreatedBy: uuid.New(), RecipientLabel: "bob",
		TokenHash: hash, VMs: []store.IngressGrantVM{{VMName: "db01", Login: "ops"}},
	})
	if err != nil {
		t.Fatalf("CreateIngressGrant() error = %v", err)
	}

	rowKey := etcd.Key("ingress_grants", g.ID.String())
	nameKey := etcd.Key("uniq", "ingress_grants", "name", strings.ToLower("Atomic-Grant"))
	tokenKey := etcd.Key("idx", "ingress_grants", "token", hex.EncodeToString(hash))

	for _, k := range []string{rowKey, nameKey, tokenKey} {
		if _, found, err := c.Get(ctx, k); err != nil || !found {
			t.Fatalf("after create, key %q present = %v (err %v), want true", k, found, err)
		}
	}

	if err := st.DeleteIngressGrant(ctx, g.ID); err != nil {
		t.Fatalf("DeleteIngressGrant() error = %v", err)
	}
	for _, k := range []string{rowKey, nameKey, tokenKey} {
		if _, found, err := c.Get(ctx, k); err != nil || found {
			t.Errorf("after delete, key %q present = %v (err %v), want false", k, found, err)
		}
	}
	// Lookups now miss.
	if _, err := st.IngressGrantByID(ctx, g.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("IngressGrantByID after delete = %v, want ErrNotFound", err)
	}
	if _, err := st.IngressGrantByTokenHash(ctx, hash); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("IngressGrantByTokenHash after delete = %v, want ErrNotFound", err)
	}
}

func TestIngressGrant_List(t *testing.T) {
	st, _ := startStore(t)
	ctx := context.Background()

	for _, name := range []string{"grant-a", "grant-b", "grant-c"} {
		if _, err := st.CreateIngressGrant(ctx, store.CreateIngressGrantParams{
			Name: name, CreatedBy: uuid.New(), TokenHash: auth.HashToken("tok-" + name),
		}); err != nil {
			t.Fatalf("CreateIngressGrant(%q) error = %v", name, err)
		}
	}
	all, err := st.ListIngressGrants(ctx, store.ListIngressGrantsParams{LimitCount: 50})
	if err != nil {
		t.Fatalf("ListIngressGrants() error = %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ListIngressGrants() returned %d grants, want 3", len(all))
	}
	// Limit caps the page.
	page, err := st.ListIngressGrants(ctx, store.ListIngressGrantsParams{LimitCount: 2})
	if err != nil {
		t.Fatalf("ListIngressGrants(limit=2) error = %v", err)
	}
	if len(page) != 2 {
		t.Errorf("ListIngressGrants(limit=2) returned %d grants, want 2", len(page))
	}
}

func loginFor(vms []store.IngressGrantVM, name string) string {
	for _, v := range vms {
		if v.VMName == name {
			return v.Login
		}
	}
	return ""
}

func vmIDFor(vms []store.IngressGrantVM, name string) uuid.UUID {
	for _, v := range vms {
		if v.VMName == name {
			return v.VMID
		}
	}
	return uuid.Nil
}

func countVM(vms []store.IngressGrantVM, name string) int {
	n := 0
	for _, v := range vms {
		if v.VMName == name {
			n++
		}
	}
	return n
}
