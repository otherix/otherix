// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Load balancers are a user-owned, cluster-wide collection addressed by UUID
// with a case-insensitive name uniqueness guard. The list is a primary-prefix
// scan rather than a secondary index because the collection is small and not on
// a hot path, mirroring networks.

func lbKey(id uuid.UUID) string { return etcd.Key("loadbalancers", id.String()) }

func lbPrefix() string { return etcd.Key("loadbalancers") + "/" }

func lbNameGuard(name string) string {
	return etcd.Key("uniq", "loadbalancers", "name", strings.ToLower(name))
}

// lbPublishedPortGuard keys the cluster-wide uniqueness guard for a load
// balancer's published (public listener) port. The guard value is the owning
// load balancer's id string.
func lbPublishedPortGuard(port int32) string {
	return etcd.Key("uniq", "lb_published_port", strconv.Itoa(int(port)))
}

// lbPublishedPortChanged reports whether an update moves the published port:
// a publish (nil->N), an unpublish (M->nil), or a swap (M->N). Equal ports
// (including both nil) return false, so an unchanged port touches no guard.
func lbPublishedPortChanged(old, cur *int32) bool {
	switch {
	case old == nil && cur == nil:
		return false
	case old == nil || cur == nil:
		return true
	default:
		return *old != *cur
	}
}

// releaseLBPublishedPortGuard drops the published-port guard for port, but only
// if it still maps to lbID. A stale in-flight update/delete holding an old read
// of the port must not blindly delete a guard a concurrent unpublish+reclaim may
// have handed to another load balancer; the value gate makes the release a
// no-op otherwise. Callers run this AFTER the main row Txn commits, so a crash
// between the two leaks a recoverable guard rather than freeing a port whose row
// is still published.
func (s *Store) releaseLBPublishedPortGuard(ctx context.Context, port int32, lbID uuid.UUID) error {
	g := lbPublishedPortGuard(port)
	_, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.Value(g), "=", lbID.String())).
		Then(clientv3.OpDelete(g)).
		Commit()
	return err
}

// claimLBPublishedPortGuard returns the compare/op to fold into the main row
// Txn to claim the published-port guard for lbID. The guard may be claimed when
// it is absent (CreateRevision==0 CAS, as normal) OR when it already points at
// lbID (a leaked guard this load balancer owns but whose row stopped
// referencing the port - reclaiming it is safe and lets the owner recover a
// wedged port). A guard held by a DIFFERENT load balancer returns
// store.ErrLoadBalancerPublishedPortExists. Returns nil/nil when the guard is
// already ours (no op needed; the row write alone re-adopts it).
func (s *Store) claimLBPublishedPortGuard(ctx context.Context, port int32, lbID uuid.UUID) ([]clientv3.Cmp, []clientv3.Op, error) {
	g := lbPublishedPortGuard(port)
	got, err := s.c.Raw().Get(ctx, g)
	if err != nil {
		return nil, nil, fmt.Errorf("read published-port guard: %v", err)
	}
	if len(got.Kvs) == 0 {
		// Absent: claim it atomically with the row (CreateRevision==0 catches a
		// concurrent claimer - the Txn fails and the caller maps it to a 409).
		return []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(g), "=", 0)},
			[]clientv3.Op{clientv3.OpPut(g, lbID.String())}, nil
	}
	if string(got.Kvs[0].Value) == lbID.String() {
		// Already ours (leaked): no guard op; the row write re-adopts it. No
		// other LB can have taken it (their CreateRevision==0 would have failed).
		return nil, nil, nil
	}
	return nil, nil, store.ErrLoadBalancerPublishedPortExists
}

// LoadBalancerByID returns the non-deleted load balancer with the given id, or
// store.ErrNotFound.
func (s *Store) LoadBalancerByID(ctx context.Context, id uuid.UUID) (store.LoadBalancer, error) {
	var lb store.LoadBalancer
	found, err := s.c.GetJSON(ctx, lbKey(id), &lb)
	if err != nil {
		return store.LoadBalancer{}, err
	}
	if !found || lb.DeletedAt != nil {
		return store.LoadBalancer{}, store.ErrNotFound
	}
	return lb, nil
}

// LoadBalancerByName resolves a non-deleted load balancer through its
// case-insensitive name guard, returning store.ErrNotFound when no live load
// balancer owns the name. The guard value is the bare id string (not JSON), so
// it is read raw and parsed.
func (s *Store) LoadBalancerByName(ctx context.Context, name string) (store.LoadBalancer, error) {
	raw, found, err := s.c.Get(ctx, lbNameGuard(name))
	if err != nil {
		return store.LoadBalancer{}, err
	}
	if !found {
		return store.LoadBalancer{}, store.ErrNotFound
	}
	id, err := uuid.Parse(string(raw))
	if err != nil {
		return store.LoadBalancer{}, fmt.Errorf("corrupt load balancer name guard %q: %v", name, err)
	}
	return s.LoadBalancerByID(ctx, id)
}

// LoadBalancerByIDWithRevision returns the non-deleted load balancer and its
// primary-row etcd ModRevision (for an optimistic-concurrency update), or
// store.ErrNotFound.
func (s *Store) LoadBalancerByIDWithRevision(ctx context.Context, id uuid.UUID) (store.LoadBalancer, int64, error) {
	got, err := s.c.Raw().Get(ctx, lbKey(id))
	if err != nil {
		return store.LoadBalancer{}, 0, err
	}
	if len(got.Kvs) == 0 {
		return store.LoadBalancer{}, 0, store.ErrNotFound
	}
	var lb store.LoadBalancer
	if err := json.Unmarshal(got.Kvs[0].Value, &lb); err != nil {
		return store.LoadBalancer{}, 0, fmt.Errorf("unmarshal load balancer %q: %v", id, err)
	}
	if lb.DeletedAt != nil {
		return store.LoadBalancer{}, 0, store.ErrNotFound
	}
	return lb, got.Kvs[0].ModRevision, nil
}

// LoadBalancerByNameWithRevision resolves a load balancer through its name guard
// and returns it with its primary-row ModRevision.
func (s *Store) LoadBalancerByNameWithRevision(ctx context.Context, name string) (store.LoadBalancer, int64, error) {
	raw, found, err := s.c.Get(ctx, lbNameGuard(name))
	if err != nil {
		return store.LoadBalancer{}, 0, err
	}
	if !found {
		return store.LoadBalancer{}, 0, store.ErrNotFound
	}
	id, err := uuid.Parse(string(raw))
	if err != nil {
		return store.LoadBalancer{}, 0, fmt.Errorf("corrupt load balancer name guard %q: %v", name, err)
	}
	return s.LoadBalancerByIDWithRevision(ctx, id)
}

// CreateLoadBalancer inserts a load balancer, stamping created_at/updated_at,
// and writes the name guard + primary atomically. A name collision
// (case-insensitive, among non-deleted rows) returns
// store.ErrLoadBalancerNameExists.
func (s *Store) CreateLoadBalancer(ctx context.Context, arg store.CreateLoadBalancerParams) (store.LoadBalancer, error) {
	now := time.Now().UTC()
	lb := store.LoadBalancer{
		ID:            arg.ID,
		Name:          arg.Name,
		OwnerID:       arg.OwnerID,
		Port:          arg.Port,
		Selector:      arg.Selector,
		HealthCheck:   arg.HealthCheck,
		PublishedPort: arg.PublishedPort,
		Protocol:      arg.Protocol,
		SourceCIDRs:   arg.SourceCIDRs,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	guard := lbNameGuard(lb.Name)
	val, err := etcd.Marshal(lb)
	if err != nil {
		return store.LoadBalancer{}, err
	}
	cmps := []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)}
	ops := []clientv3.Op{
		clientv3.OpPut(guard, lb.ID.String()),
		clientv3.OpPut(lbKey(lb.ID), string(val)),
	}
	// When published, claim the port guard (absent -> CAS; already-ours -> reclaim).
	if lb.PublishedPort != nil {
		pcmps, pops, perr := s.claimLBPublishedPortGuard(ctx, *lb.PublishedPort, lb.ID)
		if perr != nil {
			return store.LoadBalancer{}, perr
		}
		cmps = append(cmps, pcmps...)
		ops = append(ops, pops...)
	}
	resp, err := s.c.Raw().Txn(ctx).If(cmps...).Then(ops...).Commit()
	if err != nil {
		return store.LoadBalancer{}, fmt.Errorf("create load balancer txn: %v", err)
	}
	if !resp.Succeeded {
		// A published-port collision already returned from claimLBPublishedPortGuard
		// before the Txn, so the only remaining Txn rejection is a name collision.
		return store.LoadBalancer{}, store.ErrLoadBalancerNameExists
	}
	return lb, nil
}

// UpdateLoadBalancer rewrites the mutable fields of an existing load balancer,
// bumps updated_at, and moves the name guard when the name changes. Returns
// store.ErrNotFound when the row is missing and store.ErrLoadBalancerNameExists
// when a rename collides with another live load balancer.
func (s *Store) UpdateLoadBalancer(ctx context.Context, arg store.UpdateLoadBalancerParams) (store.LoadBalancer, error) {
	existing, err := s.LoadBalancerByID(ctx, arg.ID)
	if err != nil {
		return store.LoadBalancer{}, err
	}
	updated := existing
	updated.Name = arg.Name
	updated.Port = arg.Port
	updated.Selector = arg.Selector
	updated.HealthCheck = arg.HealthCheck
	updated.PublishedPort = arg.PublishedPort
	updated.Protocol = arg.Protocol
	updated.SourceCIDRs = arg.SourceCIDRs
	updated.UpdatedAt = time.Now().UTC()

	val, err := etcd.Marshal(updated)
	if err != nil {
		return store.LoadBalancer{}, err
	}

	// Main Txn: rewrite the row, swap the name guard on rename, and claim a new
	// published port (absent -> CreateRevision==0 CAS; already-ours -> reclaim).
	// The row must reflect the new port only if the new port was claimable, so
	// the claim is atomic with the write.
	cmps := []clientv3.Cmp{}
	ops := []clientv3.Op{clientv3.OpPut(lbKey(arg.ID), string(val))}

	// Optimistic concurrency: pin the row to the revision the caller read, so a
	// concurrent writer that slipped in between the caller's read and this commit
	// loses the Txn and the caller re-reads. Folded into the same Txn as the guard
	// claim, so a CAS-losing attempt also rolls back any port-guard claim it made.
	if arg.ExpectedRevision > 0 {
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(lbKey(arg.ID)), "=", arg.ExpectedRevision))
	}

	renamed := !strings.EqualFold(existing.Name, arg.Name)
	if renamed {
		newGuard := lbNameGuard(arg.Name)
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(newGuard), "=", 0))
		ops = append(ops, clientv3.OpDelete(lbNameGuard(existing.Name)), clientv3.OpPut(newGuard, arg.ID.String()))
	}

	oldPort, newPort := existing.PublishedPort, updated.PublishedPort
	portChanged := lbPublishedPortChanged(oldPort, newPort)
	if portChanged && newPort != nil {
		pcmps, pops, perr := s.claimLBPublishedPortGuard(ctx, *newPort, arg.ID)
		if perr != nil {
			return store.LoadBalancer{}, perr
		}
		cmps = append(cmps, pcmps...)
		ops = append(ops, pops...)
	}

	resp, err := s.c.Raw().Txn(ctx).If(cmps...).Then(ops...).Commit()
	if err != nil {
		return store.LoadBalancer{}, fmt.Errorf("update load balancer txn: %v", err)
	}
	if !resp.Succeeded {
		return store.LoadBalancer{}, s.classifyUpdateTxnFailure(ctx, arg)
	}

	// Release the OLD published-port guard (value-gated, after the main Txn).
	if portChanged && oldPort != nil {
		if err := s.releaseLBPublishedPortGuard(ctx, *oldPort, arg.ID); err != nil {
			return store.LoadBalancer{}, fmt.Errorf("release old published-port guard: %v", err)
		}
	}
	return updated, nil
}

// classifyUpdateTxnFailure maps a lost UpdateLoadBalancer Txn to the sentinel the
// handler expects. When a row CAS was requested, a concurrent delete or a row
// revision that moved under us is the retryable store.ErrLoadBalancerConflict
// (the handler re-reads: a re-read then 404s cleanly on a delete, or re-applies
// against the fresh row). A published-port collision already returned from
// claimLBPublishedPortGuard before the Txn, so the only other rejection is a
// name-guard collision.
func (s *Store) classifyUpdateTxnFailure(ctx context.Context, arg store.UpdateLoadBalancerParams) error {
	if arg.ExpectedRevision > 0 {
		_, curRev, rerr := s.LoadBalancerByIDWithRevision(ctx, arg.ID)
		if errors.Is(rerr, store.ErrNotFound) || (rerr == nil && curRev != arg.ExpectedRevision) {
			return store.ErrLoadBalancerConflict
		}
	}
	return store.ErrLoadBalancerNameExists
}

// ListLoadBalancers returns the non-deleted load balancers ordered by
// (created_at, id) ascending, after the cursor, capped at LimitCount. Bounded
// collection, so a primary-prefix scan with in-application filter/sort/paginate
// is used.
func (s *Store) ListLoadBalancers(ctx context.Context, arg store.ListLoadBalancersParams) ([]store.LoadBalancer, error) {
	items, err := s.c.Range(ctx, lbPrefix())
	if err != nil {
		return nil, err
	}
	lbs := make([]store.LoadBalancer, 0, len(items))
	for _, kv := range items {
		var lb store.LoadBalancer
		if err := json.Unmarshal(kv.Value, &lb); err != nil {
			return nil, fmt.Errorf("unmarshal load balancer %q: %v", kv.Key, err)
		}
		if lb.DeletedAt != nil {
			continue
		}
		if !afterCursor(lb.CreatedAt, lb.ID, arg.CursorCreatedAt, arg.CursorID) {
			continue
		}
		lbs = append(lbs, lb)
	}
	sort.Slice(lbs, func(i, j int) bool {
		if !lbs[i].CreatedAt.Equal(lbs[j].CreatedAt) {
			return lbs[i].CreatedAt.Before(lbs[j].CreatedAt)
		}
		return lbs[i].ID.String() < lbs[j].ID.String()
	})
	if n := int(arg.LimitCount); n > 0 && len(lbs) > n {
		lbs = lbs[:n]
	}
	return lbs, nil
}

// DeleteLoadBalancer soft-deletes the load balancer (sets deleted_at, drops the
// name guard so the name is reusable) after verifying it exists. A load balancer
// has no dependants, so the delete is unconditional (no blocking-resource
// check). Returns store.ErrNotFound when missing.
func (s *Store) DeleteLoadBalancer(ctx context.Context, id uuid.UUID) error {
	existing, err := s.LoadBalancerByID(ctx, id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	existing.DeletedAt = &now
	val, err := etcd.Marshal(existing)
	if err != nil {
		return err
	}
	guard := lbNameGuard(existing.Name)
	rowPut := clientv3.OpPut(lbKey(id), string(val))
	// Delete the name guard ONLY if it still points at this load balancer. A
	// concurrent delete may have already freed the name and a new load balancer
	// re-taken it; deleting that guard would orphan the new one's name. Gate on
	// the guard value; the row soft-delete (id-keyed, no cross-row race) runs in
	// both branches.
	if _, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.Value(guard), "=", id.String())).
		Then(rowPut, clientv3.OpDelete(guard)).
		Else(rowPut).
		Commit(); err != nil {
		return fmt.Errorf("delete load balancer txn: %v", err)
	}

	// Release the published-port guard (value-gated, after the soft-delete
	// commits; same stale-read defense as the name guard).
	if existing.PublishedPort != nil {
		if err := s.releaseLBPublishedPortGuard(ctx, *existing.PublishedPort, id); err != nil {
			return fmt.Errorf("release published-port guard: %v", err)
		}
	}

	// Reap this load balancer's observed backend-health records. Best-effort
	// AFTER the soft-delete has already committed: a health record has no
	// dependants and re-derives from heartbeat, so a failed reap only leaves
	// stale rows the connect path already stale-ignores. A cascade error must
	// NOT surface as a 500 on an otherwise-successful delete - log WARN and
	// return nil (the delete succeeded).
	if err := s.deleteLBBackendHealthPrefix(ctx, id); err != nil {
		s.log.WarnContext(ctx, "delete lb backend health cascade failed (delete still succeeded)",
			"lb", id.String(), "error", err.Error())
	}
	return nil
}

// ListVMsByOwner returns all of the owner's non-deleted VMs. This is a full
// unpaginated scan intended for internal load-balancer selector resolution (the
// connect path needs every match, not a page); it is distinct from the dormant,
// currently-unused store.ListVMsByOwnerParams cursor type.
func (s *Store) ListVMsByOwner(ctx context.Context, ownerID uuid.UUID) ([]store.VM, error) {
	items, err := s.c.Range(ctx, vmPrefix())
	if err != nil {
		return nil, err
	}
	out := make([]store.VM, 0, len(items))
	for _, kv := range items {
		var vm store.VM
		if err := json.Unmarshal(kv.Value, &vm); err != nil {
			return nil, fmt.Errorf("unmarshal vm %q: %v", kv.Key, err)
		}
		if vm.DeletedAt != nil || vm.OwnerID != ownerID {
			continue
		}
		out = append(out, vm)
	}
	return out, nil
}
