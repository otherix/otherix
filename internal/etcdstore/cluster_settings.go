// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/json"
	"errors"
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
	return errors.New("cas cluster settings: retries exhausted")
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

// SetDefaultArtifactPoolName writes the cluster-wide default artifact pool name
// on the singleton. The caller validates the name resolves to an existing
// artifact pool before calling.
func (s *Store) SetDefaultArtifactPoolName(ctx context.Context, name *string) error {
	return s.casClusterSettings(ctx, func(cs *store.ClusterSetting) {
		cs.DefaultArtifactPoolName = name
	})
}

// ClearDefaultArtifactPoolName nulls the cluster-wide default artifact pool
// name. Idempotent.
func (s *Store) ClearDefaultArtifactPoolName(ctx context.Context) error {
	return s.casClusterSettings(ctx, func(cs *store.ClusterSetting) {
		cs.DefaultArtifactPoolName = nil
	})
}

// SetDefaultNetworkName writes the cluster-wide default network name on the
// singleton. The caller validates the name resolves to an existing bridge
// network before calling.
func (s *Store) SetDefaultNetworkName(ctx context.Context, name *string) error {
	return s.casClusterSettings(ctx, func(cs *store.ClusterSetting) {
		cs.DefaultNetworkName = name
	})
}

// ClearDefaultNetworkName nulls the cluster-wide default network name.
// Idempotent.
func (s *Store) ClearDefaultNetworkName(ctx context.Context) error {
	return s.casClusterSettings(ctx, func(cs *store.ClusterSetting) {
		cs.DefaultNetworkName = nil
	})
}

// SeedDefaultPoolName writes the cluster-wide default pool name on the singleton
// first-writer-wins: it sets the name only when none exists, so a re-boot or a
// second replica observing an existing value (or an operator-set default) no-ops.
// An empty name is a no-op (the operator opted out of an auto-provisioned default
// pool). Unlike SetDefaultPoolName (a mutator) this never overwrites an existing
// value. The write goes through the mod-revision CAS in casClusterSettings, and
// the inner nil-recheck collapses a same-field race to a clean no-op.
func (s *Store) SeedDefaultPoolName(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	cur, err := s.ClusterSettings(ctx)
	if err != nil {
		return err
	}
	if cur.DefaultPoolName != nil {
		return nil // already set (seed or operator) - immutable from the seed's view
	}
	n := name
	return s.casClusterSettings(ctx, func(cs *store.ClusterSetting) {
		if cs.DefaultPoolName == nil {
			cs.DefaultPoolName = &n
		}
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
func (s *Store) SeedVNIRange(ctx context.Context, lo, hi int) error {
	cur, err := s.ClusterSettings(ctx)
	if err != nil {
		return err
	}
	if cur.VNIMin != nil && cur.VNIMax != nil {
		return nil // already seeded, immutable - ignore this replica's local config
	}
	if lo == 0 && hi == 0 {
		lo, hi = defaultVNIMin, defaultVNIMax
	}
	if lo < 1000 || hi > 16777215 || lo >= hi {
		return fmt.Errorf("invalid vni range [%d,%d]: require 1000<=min<max<=16777215", lo, hi)
	}
	mn := int32(lo) //nolint:gosec // bounded by the validation above
	mx := int32(hi) //nolint:gosec // bounded by the validation above
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
	lo := int32(defaultVNIMin)
	hi := int32(defaultVNIMax)
	if cs.VNIMin != nil {
		lo = *cs.VNIMin
	}
	if cs.VNIMax != nil {
		hi = *cs.VNIMax
	}
	return lo, hi, nil
}

// defaultUnderlayMTU is the physical underlay MTU assumed when the operator
// configured none at bootstrap (the classic 1500-byte Ethernet underlay).
const defaultUnderlayMTU = 1500

// minUnderlayMTU floors the seedable underlay MTU. The overlay inner MTU derives
// as underlay - store.OverlayEncapOverhead; flooring the underlay at
// OverlayEncapOverhead + 1280 keeps that derived overlay MTU at or above the
// 1280-byte IPv6 minimum link MTU (RFC 8200).
const minUnderlayMTU = int(store.OverlayEncapOverhead) + 1280 // 1390

// MinUnderlayMTU is the exported form of the underlay MTU floor (1390) so the
// boot path can WARN on a seeded value below it. The floor was 1280 before it was
// raised; a cluster seeded under an older binary can carry a value in [1280,1389]
// that derives a sub-1280 overlay MTU, below the RFC 8200 IPv6 minimum link MTU.
const MinUnderlayMTU = int32(minUnderlayMTU)

// UnderlayMTUBelowFloor reports whether a seeded underlay MTU is strictly below
// the MinUnderlayMTU floor. It backs the boot-time WARN: UnderlayMTU returns a
// legacy seed verbatim (the seed is immutable and must not be silently clamped up,
// which would push the derived overlay MTU too high and fragment), so a
// below-floor seed is surfaced as visibility, not corrected. The default 1500 and
// the floor itself return false.
func UnderlayMTUBelowFloor(mtu int32) bool { return mtu < MinUnderlayMTU }

// DerivedOverlayMTU is the overlay inner MTU that derives from a given underlay
// MTU (underlay - OverlayEncapOverhead). It lets the boot WARN name the actual
// derived overlay MTU a below-floor seed produces.
func DerivedOverlayMTU(underlay int32) int32 { return underlay - store.OverlayEncapOverhead }

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

// defaultDrainTimeoutSeconds bounds a node drain when the operator set no
// override; defaultDrainMaxConcurrentMigrations caps concurrent evacuation
// migrations per drain.
const (
	defaultDrainTimeoutSeconds          = 600
	defaultDrainMaxConcurrentMigrations = 2
)

// DrainTimeoutSeconds returns the operator-set node-drain timeout in seconds,
// falling back to the default when the singleton has no value.
func (s *Store) DrainTimeoutSeconds(ctx context.Context) (int32, error) {
	cs, err := s.ClusterSettings(ctx)
	if err != nil {
		return 0, err
	}
	if cs.DrainTimeoutSeconds != nil {
		return *cs.DrainTimeoutSeconds, nil
	}
	return defaultDrainTimeoutSeconds, nil
}

// DrainMaxConcurrentMigrations returns the operator-set cap on concurrent
// evacuation migrations per node drain, falling back to the default when the
// singleton has no value.
func (s *Store) DrainMaxConcurrentMigrations(ctx context.Context) (int32, error) {
	cs, err := s.ClusterSettings(ctx)
	if err != nil {
		return 0, err
	}
	if cs.DrainMaxConcurrentMigrations != nil {
		return *cs.DrainMaxConcurrentMigrations, nil
	}
	return defaultDrainMaxConcurrentMigrations, nil
}

// SSHIngressEnabled returns the cluster-wide SSH-ingress master switch, false
// (disabled) when the singleton has never been written. It gates whether VM
// create provisions the guest to trust the cluster SSH user-CA.
func (s *Store) SSHIngressEnabled(ctx context.Context) (bool, error) {
	cs, err := s.ClusterSettings(ctx)
	if err != nil {
		return false, err
	}
	return cs.SSHIngressEnabled, nil
}

// SetSSHIngressEnabled writes the cluster-wide SSH-ingress master switch on the
// singleton, upserting the row through the mod-revision CAS so a concurrent
// sibling-field write is not clobbered.
func (s *Store) SetSSHIngressEnabled(ctx context.Context, enabled bool) error {
	return s.casClusterSettings(ctx, func(cs *store.ClusterSetting) {
		cs.SSHIngressEnabled = enabled
	})
}

// SSHClusterSuffix returns the operator-set DNS suffix SSH-ingress VM hostnames
// are addressed under, "" when the singleton has no value (the operator must set
// it before the connector bundle / cert-mint can address a VM).
func (s *Store) SSHClusterSuffix(ctx context.Context) (string, error) {
	cs, err := s.ClusterSettings(ctx)
	if err != nil {
		return "", err
	}
	if cs.SSHClusterSuffix != nil {
		return *cs.SSHClusterSuffix, nil
	}
	return "", nil
}

// SetSSHClusterSuffix writes the SSH-ingress DNS suffix on the singleton; a nil
// suffix clears it. The write goes through the mod-revision CAS so a concurrent
// sibling-field write is not clobbered.
func (s *Store) SetSSHClusterSuffix(ctx context.Context, suffix *string) error {
	return s.casClusterSettings(ctx, func(cs *store.ClusterSetting) {
		cs.SSHClusterSuffix = suffix
	})
}
