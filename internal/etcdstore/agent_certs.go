// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package etcdstore

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/otherix/otherix/internal/etcd"
	"github.com/otherix/otherix/internal/store"
)

// Agent certs are addressed by UUID, with a per-node active index (backing the
// NodeHasActiveCert join-conflict check; entries are removed on revocation) and
// a by-fingerprint index (backing the mTLS authn middleware's cert lookup). The
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

// NodeHasActiveCert reports whether the node holds a non-revoked agent cert. A
// revoked cert does not block reuse of the node row.
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
