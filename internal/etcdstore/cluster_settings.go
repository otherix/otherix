// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Cluster settings are a singleton (the SQL schema seeds one row at id=1 with a
// NULL default_pool_name). There is no etcd equivalent of the seed migration,
// so ClusterSettings materialises the default on read when the key is absent -
// matching the SQL contract that GetClusterSettings never returns not-found in
// a healthy schema. Writes upsert the singleton key.

func clusterSettingsKey() string { return etcd.Key("cluster_settings", "singleton") }

// clusterSettingsCASRetries bounds the compare-on-mod-revision retry loop so a
// pathological write contention cannot spin forever; far more than the handful
// of distinct fields any realistic concurrent boot can race on.
const clusterSettingsCASRetries = 64

// clusterSettingsWithRev reads the singleton and its current mod-revision (the
// CAS compare target). found is false (rev 0) when the key is absent, so the
// caller's compare against ModRevision==0 behaves as create-if-absent.
func (s *Store) clusterSettingsWithRev(ctx context.Context) (cs store.ClusterSetting, rev int64, found bool, err error) {
	resp, err := s.c.Raw().Get(ctx, clusterSettingsKey())
	if err != nil {
		return store.ClusterSetting{}, 0, false, fmt.Errorf("get cluster settings: %v", err)
	}
	if len(resp.Kvs) == 0 {
		return store.ClusterSetting{}, 0, false, nil
	}
	if err := json.Unmarshal(resp.Kvs[0].Value, &cs); err != nil {
		return store.ClusterSetting{}, 0, false, fmt.Errorf("unmarshal cluster settings: %v", err)
	}
	return cs, resp.Kvs[0].ModRevision, true, nil
}

// casClusterSettings applies mutate to the singleton under a bounded
// compare-on-mod-revision retry, so concurrent writers of different fields
// serialize and merge instead of clobbering the whole object. mutate must be
// idempotent under retry; first-writer-wins callers re-check their field inside
// mutate so a lost race collapses to a clean no-op.
func (s *Store) casClusterSettings(ctx context.Context, mutate func(*store.ClusterSetting)) error {
	for range clusterSettingsCASRetries {
		cur, rev, found, err := s.clusterSettingsWithRev(ctx)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if !found {
			cur = store.ClusterSetting{ID: 1, CreatedAt: now}
		}
		cur.ID = 1
		mutate(&cur)
		if cur.CreatedAt.IsZero() {
			cur.CreatedAt = now
		}
		cur.UpdatedAt = now
		val, err := etcd.Marshal(cur)
		if err != nil {
			return err
		}
		// rev == 0 when the key is absent -> the compare behaves as create-if-absent.
		resp, err := s.c.Raw().Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(clusterSettingsKey()), "=", rev)).
			Then(clientv3.OpPut(clusterSettingsKey(), string(val))).
			Commit()
		if err != nil {
			return fmt.Errorf("cas cluster settings: %v", err)
		}
		if resp.Succeeded {
			return nil
		}
	}
	return fmt.Errorf("cas cluster settings: retries exhausted")
}

// ClusterSettings returns the singleton cluster-settings row, materialising the
// default (id=1, no default pool) when the key has never been written.
func (s *Store) ClusterSettings(ctx context.Context) (store.ClusterSetting, error) {
	var cs store.ClusterSetting
	found, err := s.c.GetJSON(ctx, clusterSettingsKey(), &cs)
	if err != nil {
		return store.ClusterSetting{}, err
	}
	if !found {
		now := time.Now().UTC()
		return store.ClusterSetting{ID: 1, CreatedAt: now, UpdatedAt: now}, nil
	}
	return cs, nil
}

// SetDefaultPoolName writes the cluster-wide default pool name on the singleton,
// upserting the row and bumping updated_at. The caller validates that the name
// resolves to at least one existing pool instance before calling.
func (s *Store) SetDefaultPoolName(ctx context.Context, name *string) error {
	return s.writeClusterSettings(ctx, name)
}

// ClearDefaultPoolName nulls the cluster-wide default pool name. Idempotent.
func (s *Store) ClearDefaultPoolName(ctx context.Context) error {
	return s.writeClusterSettings(ctx, nil)
}

// writeClusterSettings upserts the singleton with the given default pool name,
// preserving created_at when the row already exists. Unlike the Seed* fields the
// default pool name is a mutator, so it always overwrites; the CAS only guards
// against clobbering a concurrent writer's other fields.
func (s *Store) writeClusterSettings(ctx context.Context, name *string) error {
	return s.casClusterSettings(ctx, func(cs *store.ClusterSetting) {
		cs.DefaultPoolName = name
	})
}

// defaultOverlaySupernet is the cluster overlay supernet used when the operator
// configured none at bootstrap.
const defaultOverlaySupernet = "10.42.0.0/16"

// SeedOverlaySupernet writes the cluster overlay supernet on the singleton
// first-writer-wins: it validates the CIDR (IPv4, prefix in [8,30]) and only sets
// it when none exists, so a re-boot or a second replica observing an existing
// value no-ops. Empty cidr falls back to the default. The value is immutable
// after this seed - there is no public mutator; a renumber is a documented
// disruptive procedure. The write goes through a compare-on-mod-revision CAS, so
// a concurrent first boot setting a different field is not clobbered; the inner
// nil-recheck collapses a same-field race to a clean no-op.
func (s *Store) SeedOverlaySupernet(ctx context.Context, cidr string) error {
	if cidr == "" {
		cidr = defaultOverlaySupernet
	}
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return fmt.Errorf("invalid overlay supernet %q: %v", cidr, err)
	}
	if !p.Addr().Is4() {
		return fmt.Errorf("overlay supernet %q must be ipv4", cidr)
	}
	if p.Bits() < 8 || p.Bits() > 30 {
		return fmt.Errorf("overlay supernet %q prefix must be in [8,30]", cidr)
	}
	cur, err := s.ClusterSettings(ctx)
	if err != nil {
		return err
	}
	if cur.OverlaySupernet != nil {
		return nil
	}
	v := p.Masked().String()
	return s.casClusterSettings(ctx, func(cs *store.ClusterSetting) {
		if cs.OverlaySupernet == nil {
			cs.OverlaySupernet = &v
		}
	})
}

// OverlaySupernet returns the cluster overlay supernet as a masked prefix,
// falling back to the default when the singleton has no value.
func (s *Store) OverlaySupernet(ctx context.Context) (netip.Prefix, error) {
	cs, err := s.ClusterSettings(ctx)
	if err != nil {
		return netip.Prefix{}, err
	}
	cidr := defaultOverlaySupernet
	if cs.OverlaySupernet != nil && *cs.OverlaySupernet != "" {
		cidr = *cs.OverlaySupernet
	}
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("corrupt overlay supernet %q: %v", cidr, err)
	}
	return p.Masked(), nil
}

// defaultVNIMin / defaultVNIMax bound the overlay VNI range when the operator
// configured none at bootstrap. The 1000 floor avoids the reserved low VNI
// range; 65535 is the conservative phase-1 ceiling (the 24-bit VXLAN max is
// 16777215, allowed if an operator widens the range).
const (
	defaultVNIMin = 1000
	defaultVNIMax = 65535
)

// SeedVNIRange writes the overlay VNI range on the singleton first-writer-wins:
// it reads the singleton and no-ops when the bounds already exist (the value is
// immutable thereafter - no public mutator), before validating this replica's
// local config (1000<=min<max<=16777215). The short-circuit precedes validation
// so a non-first-writer replica booting with a typo'd or stale local config does
// not fail - the immutable seeded value already governs. Zero/zero falls back to
// the defaults. A FIRST seed with an invalid range still errors.
func (s *Store) SeedVNIRange(ctx context.Context, min, max int) error {
	cur, err := s.ClusterSettings(ctx)
	if err != nil {
		return err
	}
	if cur.VNIMin != nil && cur.VNIMax != nil {
		return nil // already seeded, immutable - ignore this replica's local config
	}
	if min == 0 && max == 0 {
		min, max = defaultVNIMin, defaultVNIMax
	}
	if min < 1000 || max > 16777215 || min >= max {
		return fmt.Errorf("invalid vni range [%d,%d]: require 1000<=min<max<=16777215", min, max)
	}
	mn := int32(min) //nolint:gosec // bounded by the validation above
	mx := int32(max) //nolint:gosec // bounded by the validation above
	return s.casClusterSettings(ctx, func(cs *store.ClusterSetting) {
		if cs.VNIMin == nil && cs.VNIMax == nil {
			cs.VNIMin = &mn
			cs.VNIMax = &mx
		}
	})
}

// VNIRange returns the overlay VNI range as (min, max), falling back to the
// defaults when the singleton has no value.
func (s *Store) VNIRange(ctx context.Context) (int32, int32, error) {
	cs, err := s.ClusterSettings(ctx)
	if err != nil {
		return 0, 0, err
	}
	min := int32(defaultVNIMin)
	max := int32(defaultVNIMax)
	if cs.VNIMin != nil {
		min = *cs.VNIMin
	}
	if cs.VNIMax != nil {
		max = *cs.VNIMax
	}
	return min, max, nil
}

// defaultUnderlayMTU is the physical underlay MTU assumed when the operator
// configured none at bootstrap (the classic 1500-byte Ethernet underlay).
const defaultUnderlayMTU = 1500

// minUnderlayMTU floors the seedable underlay MTU. The overlay inner MTU derives
// as underlay - store.OverlayEncapOverhead; flooring the underlay at
// OverlayEncapOverhead + 1280 keeps that derived overlay MTU at or above the
// 1280-byte IPv6 minimum link MTU (RFC 8200).
const minUnderlayMTU = int(store.OverlayEncapOverhead) + 1280 // 1390

// SeedUnderlayMTU writes the physical underlay MTU on the singleton
// first-writer-wins: it reads the singleton and no-ops when a value already
// exists (immutable thereafter - no public mutator), before validating this
// replica's local config. The short-circuit precedes validation so a
// non-first-writer replica booting with a typo'd or stale local config does not
// fail - the immutable seeded value already governs. Zero falls back to the
// default. The overlay inner MTU (underlay - OverlayEncapOverhead) and otwg0 MTU
// derive from it; the lower bound (minUnderlayMTU = 1390) keeps that derived
// overlay MTU at or above the 1280-byte IPv6 minimum link MTU. A FIRST seed with
// an out-of-range value still errors.
func (s *Store) SeedUnderlayMTU(ctx context.Context, mtu int) error {
	cur, err := s.ClusterSettings(ctx)
	if err != nil {
		return err
	}
	if cur.UnderlayMTU != nil {
		return nil // already seeded, immutable - ignore this replica's local config
	}
	if mtu == 0 {
		mtu = defaultUnderlayMTU
	}
	if mtu < minUnderlayMTU || mtu > 65535 {
		return fmt.Errorf("invalid underlay mtu %d: require %d<=mtu<=65535 so the derived overlay mtu (underlay-%d) clears the 1280-byte ipv6 minimum link mtu",
			mtu, minUnderlayMTU, store.OverlayEncapOverhead)
	}
	m := int32(mtu) //nolint:gosec // bounded by the validation above
	return s.casClusterSettings(ctx, func(cs *store.ClusterSetting) {
		if cs.UnderlayMTU == nil {
			cs.UnderlayMTU = &m
		}
	})
}

// UnderlayMTU returns the seeded underlay MTU, falling back to the default when
// the singleton has no value.
func (s *Store) UnderlayMTU(ctx context.Context) (int32, error) {
	cs, err := s.ClusterSettings(ctx)
	if err != nil {
		return 0, err
	}
	if cs.UnderlayMTU != nil {
		return *cs.UnderlayMTU, nil
	}
	return defaultUnderlayMTU, nil
}
