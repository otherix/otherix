// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Agent certs are addressed by UUID, with a per-node active index (backing the
// NodeHasActiveCert query; entries are removed on revocation) and a
// by-fingerprint index (backing the mTLS authn middleware's cert lookup). The
// cert PEM is never stored - only metadata, mirroring the SQL schema.

func agentCertKey(id uuid.UUID) string { return etcd.Key("agent_certs", id.String()) }

func agentCertNodeIndexKey(nodeID, id uuid.UUID) string {
	return etcd.Key("index", "agent_certs", "node", nodeID.String(), id.String())
}

func agentCertNodeIndexPrefix(nodeID uuid.UUID) string {
	return etcd.Key("index", "agent_certs", "node", nodeID.String()) + "/"
}

func agentCertFingerprintIndexKey(fingerprint []byte) string {
	return etcd.Key("index", "agent_certs", "fingerprint", hex.EncodeToString(fingerprint))
}

// NodeHasActiveCert reports whether the node holds a non-revoked agent cert.
// Node-join reuse is gated on the heartbeat confirm signal, not on this query
// (see upsertJoinNode); it remains a store query for callers and tests.
func (s *Store) NodeHasActiveCert(ctx context.Context, nodeID uuid.UUID) (bool, error) {
	n, err := s.countPrefix(ctx, agentCertNodeIndexPrefix(nodeID))
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// agentCertWriteOps builds the put operations that persist an agent cert: the
// primary plus the active-node and fingerprint indexes. Used inside the
// redemption transaction so the cert and the consumption audit commit together.
func agentCertWriteOps(c store.AgentCert) ([]clientv3.Op, error) {
	val, err := etcd.Marshal(c)
	if err != nil {
		return nil, err
	}
	return []clientv3.Op{
		clientv3.OpPut(agentCertKey(c.ID), string(val)),
		clientv3.OpPut(agentCertNodeIndexKey(c.NodeID, c.ID), c.ID.String()),
		clientv3.OpPut(agentCertFingerprintIndexKey(c.FingerprintSha256), c.ID.String()),
	}, nil
}

// CreateAgentCert persists agent cert metadata (primary + node + fingerprint
// indexes), stamping issued_at.
func (s *Store) CreateAgentCert(ctx context.Context, arg store.CreateAgentCertParams) (store.AgentCert, error) {
	c := store.AgentCert{
		ID:                arg.ID,
		NodeID:            arg.NodeID,
		Serial:            arg.Serial,
		FingerprintSha256: arg.FingerprintSha256,
		SubjectDn:         arg.SubjectDn,
		NotBefore:         arg.NotBefore,
		NotAfter:          arg.NotAfter,
		IssuedAt:          time.Now().UTC(),
	}
	ops, err := agentCertWriteOps(c)
	if err != nil {
		return store.AgentCert{}, err
	}
	if _, err := s.c.Raw().Txn(ctx).Then(ops...).Commit(); err != nil {
		return store.AgentCert{}, err
	}
	return c, nil
}

// revokeNodeAgentCertsOps builds the ops that revoke every agent cert issued to
// nodeID: it stamps revoked_at (+ the caller-supplied reason) on each primary
// agent_certs row for the audit trail, and deletes both the fingerprint index (so
// the mTLS lookup misses and the old leaf can no longer authenticate) and the node
// index (so NodeHasActiveCert drops to false and no index leaks). Used by the
// DeleteNode cascade (reason "node deleted") and by node-join re-enrollment to
// supersede a stale undelivered leaf (reason "superseded (re-enrollment)"); in
// both cases the ops are folded into the caller's txn so revocation commits
// atomically. count is the number of primary certs revoked.
//
// Per-cert op ORDER IS LOAD-BEARING and must not be reordered: for each cert the
// node-index delete is emitted LAST. The whole cascade routes through
// commitInChunks, and on a retry this function re-derives its work by ranging
// the node-index prefix. If the node-index delete preceded the fingerprint-index
// delete and a chunk boundary + crash fell between them, the cert would become
// unlistable on retry (node row still present -> DeleteNode re-runs -> Range no
// longer sees it), stranding the fingerprint index forever - which would keep a
// deleted node's cert authenticating, silently reopening the exact hole this
// closes. Keep node-index delete last per cert; the fail-closed property depends
// on it.
//
// Known residual: the node-index Range here is not CAS-guarded against a
// concurrent cert writer. If a node with no active cert is deleted while a
// legitimate re-join for the same name redeems a token in the Range->commit
// window, CreateAgentCert can add a fresh cert the snapshot missed, leaving a
// non-revoked fingerprint index on the soft-deleted node. The leak is
// recoverable and low-utility (the cert resolves to a soft-deleted node, so
// heartbeat name->UUID resolution fails), and closing it would add guard logic
// to the destructive delete cascade, so it is documented rather than fixed.
func (s *Store) revokeNodeAgentCertsOps(ctx context.Context, nodeID uuid.UUID, revokedAt time.Time, reason string) ([]clientv3.Op, int64, error) {
	items, err := s.c.Range(ctx, agentCertNodeIndexPrefix(nodeID))
	if err != nil {
		return nil, 0, err
	}
	var (
		ops   []clientv3.Op
		count int64
	)
	for _, kv := range items {
		certID, err := uuid.Parse(string(kv.Value))
		if err != nil {
			return nil, 0, fmt.Errorf("parse agent cert id from node index: %v", err)
		}
		var c store.AgentCert
		ok, err := s.c.GetJSON(ctx, agentCertKey(certID), &c)
		if err != nil {
			return nil, 0, err
		}
		if !ok {
			// Stale index entry with no primary: drop the index entry.
			ops = append(ops, clientv3.OpDelete(kv.Key))
			continue
		}
		c.RevokedAt = &revokedAt
		c.RevocationReason = &reason
		val, err := etcd.Marshal(c)
		if err != nil {
			return nil, 0, err
		}
		ops = append(ops,
			clientv3.OpPut(agentCertKey(certID), string(val)),
			clientv3.OpDelete(agentCertFingerprintIndexKey(c.FingerprintSha256)),
			clientv3.OpDelete(kv.Key),
		)
		count++
	}
	return ops, count, nil
}

// AgentCertByFingerprint returns the agent cert with the given SHA-256
// fingerprint regardless of revocation state (the caller inspects revoked_at),
// or store.ErrNotFound. Backs the agent-mTLS fingerprint -> node binding.
func (s *Store) AgentCertByFingerprint(ctx context.Context, fingerprint []byte) (store.AgentCert, error) {
	id, found, err := s.resolveGuard(ctx, agentCertFingerprintIndexKey(fingerprint))
	if err != nil {
		return store.AgentCert{}, err
	}
	if !found {
		return store.AgentCert{}, store.ErrNotFound
	}
	var c store.AgentCert
	ok, err := s.c.GetJSON(ctx, agentCertKey(id), &c)
	if err != nil {
		return store.AgentCert{}, err
	}
	if !ok {
		return store.AgentCert{}, store.ErrNotFound
	}
	return c, nil
}
