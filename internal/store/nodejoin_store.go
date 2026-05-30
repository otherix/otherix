// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package store

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Join-token redemption rejection sentinels. The /v1/nodes/join handler
// type-switches on these to emit the right HTTP envelope; token-unknown,
// expired, and exhausted intentionally collapse to identical 401
// responses at the edge (no info leak).
var (
	// ErrJoinTokenInvalid reports an unknown or expired join token. The
	// hash lookup filters expired tokens, so unknown and expired collapse
	// to this single sentinel.
	ErrJoinTokenInvalid = errors.New("store: join token unknown or expired")
	// ErrJoinTokenExhausted reports that the token's max_uses cap would be
	// exceeded by this redemption.
	ErrJoinTokenExhausted = errors.New("store: join token max_uses exceeded")
	// ErrJoinNodeNameMismatch reports a pre-bound token redeemed for a node
	// name other than its intended_node_name.
	ErrJoinNodeNameMismatch = errors.New("store: node name does not match token binding")
	// ErrJoinNodeNameTaken reports that the target node already holds an
	// active (non-revoked) cert; the operator must revoke before
	// re-bootstrapping.
	ErrJoinNodeNameTaken = errors.New("store: node already has an active cert")
)

// RedeemJoinTokenParams carries the already-validated redemption inputs.
// Request-shape and CSR validation are the caller's responsibility; this
// struct holds only what the atomic transaction needs.
type RedeemJoinTokenParams struct {
	TokenHash               []byte
	NodeName                string
	Architecture            CPUArch
	AdvertisedEndpoint      string
	MigrationHost           string
	MigrationPortRangeStart int32
	MigrationPortRangeEnd   int32
	SourceIP                *netip.Addr
}

// IssuedCert is the metadata the redemption persists for a freshly
// signed agent cert. The signing itself (x509 / crypto) lives in the
// caller's sign callback so the store stays driver-and-crypto-agnostic;
// only the resulting metadata crosses back in.
type IssuedCert struct {
	Serial            []byte
	FingerprintSha256 []byte
	SubjectDN         string
	NotBefore         time.Time
	NotAfter          time.Time
}

// RedeemJoinTokenResult reports the ids touched by a successful
// redemption, for the caller's audit logging after commit.
type RedeemJoinTokenResult struct {
	NodeID  uuid.UUID
	TokenID uuid.UUID
}

// RedeemJoinToken runs the atomic Step 2 redemption transaction:
//
//  1. Acquire a row lock on the join_tokens row (SELECT FOR UPDATE).
//  2. Enforce the max_uses cap against the consumption count.
//  3. Enforce the intended_node_name binding when present.
//  4. Look up or create the nodes row (rejecting a name that already
//     holds an active cert).
//  5. Invoke sign(node) to obtain the signed-cert metadata. sign runs
//     inside the transaction with the resolved node so the cert binds to
//     the right identity; an error from sign rolls the whole redemption
//     back and is returned unwrapped so callers can inspect it.
//  6. Insert the agent_certs metadata row and the
//     join_token_consumptions audit row.
//
// Any failure rolls back the entire transaction. The four domain
// rejections surface as ErrJoinTokenInvalid / ErrJoinTokenExhausted /
// ErrJoinNodeNameMismatch / ErrJoinNodeNameTaken.
func (s *Store) RedeemJoinToken(ctx context.Context, p RedeemJoinTokenParams, sign func(node Node) (IssuedCert, error)) (RedeemJoinTokenResult, error) {
	var result RedeemJoinTokenResult
	err := s.InTx(ctx, func(q *Queries) error {
		token, err := q.GetJoinTokenByHashForUpdate(ctx, p.TokenHash)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrJoinTokenInvalid
			}
			return fmt.Errorf("token lookup: %v", err)
		}

		if token.MaxUses != nil {
			count, err := q.CountJoinTokenConsumptions(ctx, token.ID)
			if err != nil {
				return fmt.Errorf("count consumptions: %v", err)
			}
			if count >= int64(*token.MaxUses) {
				return ErrJoinTokenExhausted
			}
		}

		if token.IntendedNodeName != nil && *token.IntendedNodeName != p.NodeName {
			return ErrJoinNodeNameMismatch
		}

		node, err := upsertJoinNode(ctx, q, p)
		if err != nil {
			return err
		}

		issued, err := sign(node)
		if err != nil {
			return err
		}

		if _, err := q.CreateAgentCert(ctx, CreateAgentCertParams{
			ID:                uuid.New(),
			NodeID:            node.ID,
			Serial:            issued.Serial,
			FingerprintSha256: issued.FingerprintSha256,
			SubjectDn:         issued.SubjectDN,
			NotBefore:         issued.NotBefore,
			NotAfter:          issued.NotAfter,
		}); err != nil {
			return fmt.Errorf("create agent cert: %v", err)
		}

		nodeID := node.ID
		if _, err := q.CreateJoinTokenConsumption(ctx, CreateJoinTokenConsumptionParams{
			ID:               uuid.New(),
			JoinTokenID:      token.ID,
			ConsumedByNodeID: &nodeID,
			SourceIp:         p.SourceIP,
		}); err != nil {
			return fmt.Errorf("create consumption: %v", err)
		}

		result.NodeID = node.ID
		result.TokenID = token.ID
		return nil
	})
	return result, err
}

// upsertJoinNode resolves the node row for a redemption: it returns the
// existing node when one already exists for the name (rejecting it with
// ErrJoinNodeNameTaken when that node still holds an active cert),
// otherwise it creates a fresh pending node in the same transaction so
// concurrent redemptions cannot race-create twins under a unique
// violation.
func upsertJoinNode(ctx context.Context, q *Queries, p RedeemJoinTokenParams) (Node, error) {
	existing, err := q.GetNodeByName(ctx, p.NodeName)
	if err == nil {
		hasActive, err := q.NodeHasActiveCert(ctx, existing.ID)
		if err != nil {
			return Node{}, fmt.Errorf("check active cert: %v", err)
		}
		if hasActive {
			return Node{}, ErrJoinNodeNameTaken
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Node{}, fmt.Errorf("node lookup: %v", err)
	}

	node, err := q.CreateNode(ctx, CreateNodeParams{
		ID:                      uuid.New(),
		Name:                    p.NodeName,
		Architecture:            p.Architecture,
		AdvertisedEndpoint:      p.AdvertisedEndpoint,
		MigrationHost:           p.MigrationHost,
		MigrationPortRangeStart: p.MigrationPortRangeStart,
		MigrationPortRangeEnd:   p.MigrationPortRangeEnd,
		Status:                  NodeStatusPending,
	})
	if err != nil {
		return Node{}, fmt.Errorf("create node: %v", err)
	}
	return node, nil
}
