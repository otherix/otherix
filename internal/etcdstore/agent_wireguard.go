// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Agent WireGuard fabric identity: one record per node at
// /otherix/agent_wireguard/<node_id>, with a pubkey uniqueness guard at
// /otherix/uniq/agent_wireguard/pubkey/<sha256(pubkey)> -> node_id and a
// monotonic overlay-index counter at /otherix/seq/wireguard_index. The CP
// assigns a flat /32 overlay address per node (supernet base + index + 1) on
// first report and freezes it.

func agentWireguardKey(nodeID uuid.UUID) string {
	return etcd.Key("agent_wireguard", nodeID.String())
}

func agentWireguardPrefix() string { return etcd.Key("agent_wireguard") + "/" }

// agentWireguardPubkeyGuard keys the pubkey uniqueness guard by sha256(pubkey)
// rather than the raw base64 key, whose '/' characters would otherwise split
// the etcd key path (mirrors agent_certs keying by the hex fingerprint).
func agentWireguardPubkeyGuard(pubkey string) string {
	sum := sha256.Sum256([]byte(pubkey))
	return etcd.Key("uniq", "agent_wireguard", "pubkey", hex.EncodeToString(sum[:]))
}

func wireguardIndexSeqKey() string { return etcd.Key("seq", "wireguard_index") }

// overlayIPForIndex computes an agent's flat /32 overlay address within the
// IPv4 supernet: address = supernet base + index + 1 (index 0 -> first host).
// It rejects an index past the supernet's host capacity (network + broadcast
// addresses excluded).
func overlayIPForIndex(supernet netip.Prefix, idx int32) (netip.Addr, error) {
	if !supernet.Addr().Is4() {
		return netip.Addr{}, fmt.Errorf("overlay supernet %s is not ipv4", supernet)
	}
	bits := supernet.Bits()
	if bits < 8 || bits > 30 {
		return netip.Addr{}, fmt.Errorf("overlay supernet %s prefix out of range [8,30]", supernet)
	}
	capacity := int64(1)<<(32-bits) - 2 // exclude network + broadcast
	if int64(idx) >= capacity {
		return netip.Addr{}, store.ErrOverlaySupernetExhausted
	}
	base4 := supernet.Masked().Addr().As4()
	base := binary.BigEndian.Uint32(base4[:])
	// idx is bounded by capacity (< 2^24 for bits >= 8), so base + idx + 1
	// cannot overflow uint32.
	v := base + uint32(idx) + 1 //nolint:gosec // idx bounded by the capacity check above
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b), nil
}

// UpsertAgentWireguard records an agent's observed WG state. First report for a
// node allocates a monotonic index + overlay IP and acquires the pubkey guard;
// a re-report keeps the index/IP and refreshes endpoint/port/peers; a changed
// pubkey swaps the guard. Returns store.ErrAgentWireguardPubkeyInUse when the
// reported pubkey is held by a different node, store.ErrOverlaySupernetExhausted
// when the supernet has no free host address.
//
// Precondition: calls for a given node_id are serialized (the heartbeat
// projection applies one agent's report at a time, like the rest of the
// projection). Two concurrent FIRST-reports for the same node would each
// allocate a distinct index and last-write-wins the primary, burning an index
// and orphaning a guard; the per-node heartbeat serialization makes that
// unreachable, and a redelivered report re-converges idempotently.
func (s *Store) UpsertAgentWireguard(ctx context.Context, arg store.UpsertAgentWireguardParams) error {
	if arg.PublicKey == "" {
		return errors.New("etcdstore: upsert agent_wireguard: empty public key")
	}
	var existing store.AgentWireguard
	found, err := s.c.GetJSON(ctx, agentWireguardKey(arg.NodeID), &existing)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if !found {
		return s.createAgentWireguard(ctx, arg, now)
	}
	rec := existing
	rec.Endpoint = arg.Endpoint
	rec.ListenPort = arg.ListenPort
	rec.EstablishedPeers = arg.EstablishedPeers
	rec.ReconciliationStatus = arg.ReconciliationStatus
	rec.ReconciliationError = arg.ReconciliationError
	rec.UpdatedAt = now
	if existing.PublicKey == arg.PublicKey {
		return s.c.PutJSON(ctx, agentWireguardKey(arg.NodeID), rec)
	}
	return s.swapAgentWireguardPubkey(ctx, existing, rec, arg.PublicKey)
}

// createAgentWireguard handles the first report for a node: allocate index +
// overlay IP, then commit the pubkey guard + primary in one CAS txn.
func (s *Store) createAgentWireguard(ctx context.Context, arg store.UpsertAgentWireguardParams, now time.Time) error {
	supernet, err := s.OverlaySupernet(ctx)
	if err != nil {
		return err
	}
	seq, err := s.nextSeq(ctx, wireguardIndexSeqKey())
	if err != nil {
		return err
	}
	// seq is a monotonic etcd counter starting at 1. The narrowing here precedes
	// the capacity check, so its safety rests on the enrollment count never
	// approaching 2^31 - the supernet's host capacity (at most ~2^24 for a
	// realistic /8..16, enforced next by overlayIPForIndex) exhausts far sooner.
	idx := int32(seq - 1) //nolint:gosec // seq is far below 2^31; supernet capacity exhausts first
	overlayIP, err := overlayIPForIndex(supernet, idx)
	if err != nil {
		return err
	}
	rec := store.AgentWireguard{
		NodeID:               arg.NodeID,
		PublicKey:            arg.PublicKey,
		Endpoint:             arg.Endpoint,
		OverlayIP:            overlayIP,
		AgentIndex:           idx,
		ListenPort:           arg.ListenPort,
		EstablishedPeers:     arg.EstablishedPeers,
		ReconciliationStatus: arg.ReconciliationStatus,
		ReconciliationError:  arg.ReconciliationError,
		UpdatedAt:            now,
	}
	val, err := etcd.Marshal(rec)
	if err != nil {
		return err
	}
	guard := agentWireguardPubkeyGuard(arg.PublicKey)
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(guard), "=", 0)).
		Then(
			clientv3.OpPut(guard, arg.NodeID.String()),
			clientv3.OpPut(agentWireguardKey(arg.NodeID), string(val)),
		).Commit()
	if err != nil {
		return err
	}
	if resp.Succeeded {
		return nil
	}
	owner, ok, err := s.resolveGuard(ctx, guard)
	if err != nil {
		return err
	}
	if ok && owner == arg.NodeID {
		return s.c.PutJSON(ctx, agentWireguardKey(arg.NodeID), rec)
	}
	return store.ErrAgentWireguardPubkeyInUse
}

// swapAgentWireguardPubkey moves a node's guard from its old pubkey to a new one
// (re-enrolled agent), keeping the frozen index/IP carried on updated.
func (s *Store) swapAgentWireguardPubkey(ctx context.Context, old, updated store.AgentWireguard, newPubkey string) error {
	updated.PublicKey = newPubkey
	val, err := etcd.Marshal(updated)
	if err != nil {
		return err
	}
	newGuard := agentWireguardPubkeyGuard(newPubkey)
	oldGuard := agentWireguardPubkeyGuard(old.PublicKey)
	resp, err := s.c.Raw().Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(newGuard), "=", 0)).
		Then(
			clientv3.OpPut(newGuard, old.NodeID.String()),
			clientv3.OpDelete(oldGuard),
			clientv3.OpPut(agentWireguardKey(old.NodeID), string(val)),
		).Commit()
	if err != nil {
		return err
	}
	if resp.Succeeded {
		return nil
	}
	owner, ok, err := s.resolveGuard(ctx, newGuard)
	if err != nil {
		return err
	}
	if ok && owner == old.NodeID {
		if _, err := s.c.Delete(ctx, oldGuard); err != nil {
			return err
		}
		return s.c.PutJSON(ctx, agentWireguardKey(old.NodeID), updated)
	}
	return store.ErrAgentWireguardPubkeyInUse
}

// AgentWireguardByNodeID returns the node's WG fabric record or store.ErrNotFound.
func (s *Store) AgentWireguardByNodeID(ctx context.Context, nodeID uuid.UUID) (store.AgentWireguard, error) {
	var rec store.AgentWireguard
	found, err := s.c.GetJSON(ctx, agentWireguardKey(nodeID), &rec)
	if err != nil {
		return store.AgentWireguard{}, err
	}
	if !found {
		return store.AgentWireguard{}, store.ErrNotFound
	}
	return rec, nil
}

// ListAgentWireguard returns every agent WG fabric record. The caller (heartbeat
// down-channel) filters out self and derives allowed_ips.
func (s *Store) ListAgentWireguard(ctx context.Context) ([]store.AgentWireguard, error) {
	return s.listAgentWireguardAtRev(ctx, 0)
}

// listAgentWireguardAtRev is ListAgentWireguard pinned to an MVCC revision
// (rev==0 reads latest). The FDB projection reads it at its snapshot revision so
// the VTEP-IP map it builds matches the placement list's snapshot.
func (s *Store) listAgentWireguardAtRev(ctx context.Context, rev int64) ([]store.AgentWireguard, error) {
	items, _, err := s.c.RangeRev(ctx, agentWireguardPrefix(), rev)
	if err != nil {
		return nil, err
	}
	out := make([]store.AgentWireguard, 0, len(items))
	for _, kv := range items {
		var rec store.AgentWireguard
		if err := json.Unmarshal(kv.Value, &rec); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}
