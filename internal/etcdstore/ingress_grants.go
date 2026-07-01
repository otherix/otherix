// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// ingress grants are a bounded, cluster-wide collection addressed by UUID with a
// case-insensitive name uniqueness guard and a token-hash secondary index for
// the connect-time lookup. The row, the name guard, and the token index are
// written and cleared in one etcd Txn so a crash never leaves a token index
// pointing at a missing row, nor a row unreachable by its token.

// ingressGrantCASRetries bounds the read-modify-write retry loop on the grant row so
// pathological write contention cannot spin forever.
const ingressGrantCASRetries = 64

func ingressGrantKey(id uuid.UUID) string { return etcd.Key("ingress_grants", id.String()) }

func ingressGrantPrefix() string { return etcd.Key("ingress_grants") + "/" }

func ingressGrantNameGuard(name string) string {
	return etcd.Key("uniq", "ingress_grants", "name", strings.ToLower(name))
}

func ingressGrantTokenIndex(hash []byte) string {
	return etcd.Key("idx", "ingress_grants", "token", hex.EncodeToString(hash))
}

// CreateIngressGrant mints a grant, stamping created_at/updated_at, and writes the
// primary row + the case-insensitive name guard + the token-hash index in one
// atomic Txn. A name collision returns store.ErrIngressGrantNameExists. The three
// keys land together, so there is never a dangling token index or a
// token-unreachable row.
func (s *Store) CreateIngressGrant(ctx context.Context, arg store.CreateIngressGrantParams) (store.IngressGrant, error) {
	now := time.Now().UTC()
	vms := arg.VMs
	if vms == nil {
		vms = []store.IngressGrantVM{}
	}
	g := store.IngressGrant{
		ID:             uuid.New(),
		Name:           arg.Name,
		CreatedBy:      arg.CreatedBy,
		RecipientLabel: arg.RecipientLabel,
		TokenHash:      arg.TokenHash,
		VMs:            vms,
		ExpiresAt:      arg.ExpiresAt,
		Revoked:        false,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	val, err := etcd.Marshal(g)
	if err != nil {
		return store.IngressGrant{}, err
	}
	guard := ingressGrantNameGuard(g.Name)
	tokenIdx := ingressGrantTokenIndex(g.TokenHash)
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(
			clientv3.OpPut(guard, g.ID.String()),
			clientv3.OpPut(tokenIdx, g.ID.String()),
			clientv3.OpPut(ingressGrantKey(g.ID), string(val)),
		).
		Commit()
	if err != nil {
		return store.IngressGrant{}, fmt.Errorf("create ingress grant txn: %v", err)
	}
	if !resp.Succeeded {
		return store.IngressGrant{}, store.ErrIngressGrantNameExists
	}
	return g, nil
}

// IngressGrantByID returns the grant with the given id, or store.ErrNotFound.
func (s *Store) IngressGrantByID(ctx context.Context, id uuid.UUID) (store.IngressGrant, error) {
	var g store.IngressGrant
	found, err := s.c.GetJSON(ctx, ingressGrantKey(id), &g)
	if err != nil {
		return store.IngressGrant{}, err
	}
	if !found {
		return store.IngressGrant{}, store.ErrNotFound
	}
	return g, nil
}

// IngressGrantByName resolves a grant through its case-insensitive name guard,
// returning store.ErrNotFound when no grant owns the name.
func (s *Store) IngressGrantByName(ctx context.Context, name string) (store.IngressGrant, error) {
	val, found, err := s.c.Get(ctx, ingressGrantNameGuard(name))
	if err != nil {
		return store.IngressGrant{}, err
	}
	if !found {
		return store.IngressGrant{}, store.ErrNotFound
	}
	id, err := uuid.Parse(string(val))
	if err != nil {
		return store.IngressGrant{}, fmt.Errorf("corrupt ingress grant name guard %q: %v", name, err)
	}
	return s.IngressGrantByID(ctx, id)
}

// IngressGrantByTokenHash resolves a grant through its token-hash index - the
// connect-time / cert-mint lookup - returning store.ErrNotFound when absent. A
// revoked grant is still returned (the row survives) so the caller can reject
// uniformly rather than leak revocation as not-found.
func (s *Store) IngressGrantByTokenHash(ctx context.Context, hash []byte) (store.IngressGrant, error) {
	val, found, err := s.c.Get(ctx, ingressGrantTokenIndex(hash))
	if err != nil {
		return store.IngressGrant{}, err
	}
	if !found {
		return store.IngressGrant{}, store.ErrNotFound
	}
	id, err := uuid.Parse(string(val))
	if err != nil {
		return store.IngressGrant{}, fmt.Errorf("corrupt ingress grant token index: %v", err)
	}
	return s.IngressGrantByID(ctx, id)
}

// ListIngressGrants returns the grants ordered by (created_at, id) ascending, after
// the cursor, capped at LimitCount. Bounded collection, so a primary-prefix scan
// with in-application sort/paginate is used (mirrors ListNetworks).
func (s *Store) ListIngressGrants(ctx context.Context, arg store.ListIngressGrantsParams) ([]store.IngressGrant, error) {
	items, err := s.c.Range(ctx, ingressGrantPrefix())
	if err != nil {
		return nil, err
	}
	grants := make([]store.IngressGrant, 0, len(items))
	for _, kv := range items {
		var g store.IngressGrant
		if err := json.Unmarshal(kv.Value, &g); err != nil {
			return nil, fmt.Errorf("unmarshal ingress grant %q: %v", kv.Key, err)
		}
		if !afterCursor(g.CreatedAt, g.ID, arg.CursorCreatedAt, arg.CursorID) {
			continue
		}
		grants = append(grants, g)
	}
	sort.Slice(grants, func(i, j int) bool {
		if !grants[i].CreatedAt.Equal(grants[j].CreatedAt) {
			return grants[i].CreatedAt.Before(grants[j].CreatedAt)
		}
		return grants[i].ID.String() < grants[j].ID.String()
	})
	if n := int(arg.LimitCount); n > 0 && len(grants) > n {
		grants = grants[:n]
	}
	return grants, nil
}

// AddIngressGrantVM adds (or replaces the login of) a VM entry in the grant's
// mutable scope under a ModRevision CAS, bumping updated_at. Idempotent on an
// existing vm.VMName: the entry's login is replaced, never duplicated.
func (s *Store) AddIngressGrantVM(ctx context.Context, grantID uuid.UUID, vm store.IngressGrantVM) (store.IngressGrant, error) {
	return s.mutateIngressGrant(ctx, grantID, func(g *store.IngressGrant) {
		for i := range g.VMs {
			if g.VMs[i].VMName == vm.VMName {
				g.VMs[i].Login = vm.Login
				return
			}
		}
		g.VMs = append(g.VMs, vm)
	})
}

// RemoveIngressGrantVM drops the VM entry with the given name from the grant's
// scope under a ModRevision CAS, bumping updated_at. Removing an absent VM is a
// no-op (still bumps updated_at).
func (s *Store) RemoveIngressGrantVM(ctx context.Context, grantID uuid.UUID, vmName string) (store.IngressGrant, error) {
	return s.mutateIngressGrant(ctx, grantID, func(g *store.IngressGrant) {
		kept := g.VMs[:0]
		for _, v := range g.VMs {
			if v.VMName != vmName {
				kept = append(kept, v)
			}
		}
		g.VMs = kept
	})
}

// RevokeIngressGrant flags the grant Revoked under a ModRevision CAS. The row and
// its token index are kept so audit and a deterministic uniform reject survive.
// Idempotent: revoking an already-revoked grant re-stamps updated_at only.
func (s *Store) RevokeIngressGrant(ctx context.Context, grantID uuid.UUID) error {
	_, err := s.mutateIngressGrant(ctx, grantID, func(g *store.IngressGrant) {
		g.Revoked = true
	})
	return err
}

// mutateIngressGrant read-modify-writes the grant row under a bounded ModRevision
// CAS retry: it reads the row + its mod-revision, applies mutate, and commits
// guarded on that revision so a concurrent writer cannot be clobbered. Returns
// store.ErrNotFound when the row is missing.
func (s *Store) mutateIngressGrant(ctx context.Context, grantID uuid.UUID, mutate func(*store.IngressGrant)) (store.IngressGrant, error) {
	key := ingressGrantKey(grantID)
	for range ingressGrantCASRetries {
		resp, err := s.c.Raw().Get(ctx, key)
		if err != nil {
			return store.IngressGrant{}, fmt.Errorf("get ingress grant: %v", err)
		}
		if len(resp.Kvs) == 0 {
			return store.IngressGrant{}, store.ErrNotFound
		}
		var g store.IngressGrant
		if err := json.Unmarshal(resp.Kvs[0].Value, &g); err != nil {
			return store.IngressGrant{}, fmt.Errorf("unmarshal ingress grant: %v", err)
		}
		rev := resp.Kvs[0].ModRevision
		mutate(&g)
		g.UpdatedAt = time.Now().UTC()
		val, err := etcd.Marshal(g)
		if err != nil {
			return store.IngressGrant{}, err
		}
		txn, err := s.c.Raw().Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(key), "=", rev)).
			Then(clientv3.OpPut(key, string(val))).
			Commit()
		if err != nil {
			return store.IngressGrant{}, fmt.Errorf("mutate ingress grant txn: %v", err)
		}
		if txn.Succeeded {
			return g, nil
		}
	}
	return store.IngressGrant{}, errors.New("mutate ingress grant: retries exhausted")
}

// DeleteIngressGrant removes the grant row, its name guard, and its token index in
// one atomic Txn. Returns store.ErrNotFound when the grant is missing. The name
// guard is dropped only if it still points at this grant, so a concurrent create
// that re-took the freed name is not orphaned.
func (s *Store) DeleteIngressGrant(ctx context.Context, grantID uuid.UUID) error {
	g, err := s.IngressGrantByID(ctx, grantID)
	if err != nil {
		return err
	}
	guard := ingressGrantNameGuard(g.Name)
	baseOps := []clientv3.Op{
		clientv3.OpDelete(ingressGrantKey(grantID)),
		clientv3.OpDelete(ingressGrantTokenIndex(g.TokenHash)),
	}
	thenOps := append(append([]clientv3.Op{}, baseOps...), clientv3.OpDelete(guard))
	if _, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.Value(guard), "=", grantID.String())).
		Then(thenOps...).
		Else(baseOps...).
		Commit(); err != nil {
		return fmt.Errorf("delete ingress grant txn: %v", err)
	}
	return nil
}
