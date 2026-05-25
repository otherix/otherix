// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package nodejoin

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/otherix/otherix/internal/auth"
	"github.com/otherix/otherix/internal/store"
)

// Redemption sentinel errors. The handler type-switches on these to
// emit the right HTTP envelope; the slog WARN line distinguishes them
// from each other for forensics.
var (
	errTokenInvalid     = errors.New("join token is unknown or expired")
	errTokenExhausted   = errors.New("join token max_uses exceeded")
	errNodeNameMismatch = errors.New("node_name does not match token's intended_node_name")
	errNodeNameTaken    = errors.New("node_name already has an active cert")
)

// caBundle is the in-memory projection of the active ca_certs row
// used by redemption. Pre-parsed cert + key to avoid re-parsing inside
// the atomic transaction.
type caBundle struct {
	cert    *x509.Certificate
	key     crypto.Signer
	certPEM []byte
}

// loadCA fetches the active cluster CA row, parses both cert and key,
// and returns a caBundle ready for SignCSR. Per-request load mirrors
// the /v1/ca handler pattern — the bootstrap
// path is infrequent enough that the parse cost is acceptable, and
// avoiding cached state simplifies CA rotation flows landing later.
func (h *Handler) loadCA(ctx context.Context) (*caBundle, error) {
	row, err := h.store.Queries().GetActiveCACert(ctx)
	if err != nil {
		return nil, fmt.Errorf("active CA lookup: %v", err)
	}
	cert, _, err := auth.ParseClusterCACert(row.CertPem)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %v", err)
	}
	rawKey, err := auth.ParseClusterCAKey(row.KeyPem)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %v", err)
	}
	signer, ok := rawKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("CA key type %T does not implement crypto.Signer", rawKey)
	}
	return &caBundle{cert: cert, key: signer, certPEM: row.CertPem}, nil
}

// redeemResult is the in-memory shape of a successful redemption,
// passed back to the handler for response construction. The full
// cert PEM lives here (and in the HTTP response); the database keeps
// only metadata.
type redeemResult struct {
	nodeID  string
	certPEM []byte
}

// redeem performs the atomic Step 2 redemption transaction. The
// caller has already validated request shape + CSR contents; this
// function executes:
//
//  1. Acquire row lock on join_tokens (SELECT FOR UPDATE).
//  2. Enforce max_uses cap against consumption count.
//  3. Enforce intended_node_name binding if present.
//  4. Lookup or create the nodes row.
//  5. Reject if existing node has an active cert (409 conflict).
//  6. Sign the CSR using the loaded CA.
//  7. Insert agent_certs metadata row.
//  8. Insert join_token_consumptions audit row.
//  9. Commit.
//
// Any failure rolls back the entire transaction — cert is NOT issued
// downstream and no observable state lands.
func (h *Handler) redeem(ctx context.Context, req joinRequest, csr *x509.CertificateRequest, ca *caBundle, sourceIP *netip.Addr) (redeemResult, error) {
	var result redeemResult

	err := h.store.InTx(ctx, func(q *store.Queries) error {
		token, err := h.lookupToken(ctx, q, req.Token)
		if err != nil {
			return err
		}

		if err := h.enforceMaxUses(ctx, q, token); err != nil {
			return err
		}

		if err := h.enforceBinding(token, req.NodeName); err != nil {
			return err
		}

		node, err := h.upsertNode(ctx, q, req)
		if err != nil {
			return err
		}

		certPEM, parsed, err := auth.SignCSR(csr, req.NodeName, ca.cert, ca.key, time.Now())
		if err != nil {
			return fmt.Errorf("sign csr: %v", err)
		}

		fingerprint := sha256.Sum256(parsed.Raw)
		if _, err := q.CreateAgentCert(ctx, store.CreateAgentCertParams{
			ID:                uuid.New(),
			NodeID:            node.ID,
			Serial:            parsed.SerialNumber.Bytes(),
			FingerprintSha256: fingerprint[:],
			SubjectDn:         parsed.Subject.String(),
			NotBefore:         parsed.NotBefore,
			NotAfter:          parsed.NotAfter,
		}); err != nil {
			return fmt.Errorf("create agent cert: %v", err)
		}

		nodeID := node.ID
		if _, err := q.CreateJoinTokenConsumption(ctx, store.CreateJoinTokenConsumptionParams{
			ID:               uuid.New(),
			JoinTokenID:      token.ID,
			ConsumedByNodeID: &nodeID,
			SourceIp:         sourceIP,
		}); err != nil {
			return fmt.Errorf("create consumption: %v", err)
		}

		h.log.InfoContext(ctx, "join token redeemed",
			slog.String("token_id", token.ID.String()),
			slog.String("node_id", node.ID.String()),
			slog.String("node_name", req.NodeName),
			slog.String("cert_fingerprint", fmt.Sprintf("%x", fingerprint)))

		result.nodeID = node.ID.String()
		result.certPEM = certPEM
		return nil
	})
	if err != nil {
		return redeemResult{}, err
	}
	return result, nil
}

// lookupToken acquires a FOR UPDATE row lock on the join_tokens row
// for race-safe max_uses enforcement. pgx.ErrNoRows ⇒ errTokenInvalid
// (the query filters expired tokens out, so unknown + expired collapse
// to the same sentinel — no info leak via differing 401 envelopes).
func (h *Handler) lookupToken(ctx context.Context, q *store.Queries, plaintext string) (store.JoinToken, error) {
	hash := auth.HashToken(plaintext)
	token, err := q.GetJoinTokenByHashForUpdate(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.JoinToken{}, errTokenInvalid
		}
		return store.JoinToken{}, fmt.Errorf("token lookup: %v", err)
	}
	return token, nil
}

// enforceMaxUses checks the cap (if non-null) against the current
// consumption count inside the held row lock. Returns errTokenExhausted
// if the cap would be exceeded.
func (h *Handler) enforceMaxUses(ctx context.Context, q *store.Queries, token store.JoinToken) error {
	if token.MaxUses == nil {
		return nil
	}
	count, err := q.CountJoinTokenConsumptions(ctx, token.ID)
	if err != nil {
		return fmt.Errorf("count consumptions: %v", err)
	}
	if count >= int64(*token.MaxUses) {
		return errTokenExhausted
	}
	return nil
}

// enforceBinding ensures a pre-bound token is being redeemed for the
// exact node_name it was minted for. Case-sensitive match — node names
// are addressed case-insensitively elsewhere (uq_nodes_name on
// lower(name)) but tokens bind to the exact string the operator
// supplied.
func (h *Handler) enforceBinding(token store.JoinToken, requestedName string) error {
	if token.IntendedNodeName == nil {
		return nil
	}
	if *token.IntendedNodeName != requestedName {
		return errNodeNameMismatch
	}
	return nil
}

// upsertNode looks up the node by name; creates a fresh `pending` row
// if absent. Returns errNodeNameTaken if the existing node already
// has an active (non-revoked) cert — the operator must revoke first
// before re-bootstrapping with a fresh keypair.
func (h *Handler) upsertNode(ctx context.Context, q *store.Queries, req joinRequest) (store.Node, error) {
	existing, err := q.GetNodeByName(ctx, req.NodeName)
	if err == nil {
		// Existing node — check for active cert collision.
		hasActive, err := q.NodeHasActiveCert(ctx, existing.ID)
		if err != nil {
			return store.Node{}, fmt.Errorf("check active cert: %v", err)
		}
		if hasActive {
			return store.Node{}, errNodeNameTaken
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return store.Node{}, fmt.Errorf("node lookup: %v", err)
	}

	// Fresh node — create in the same TX so concurrent redemptions can't
	// race-create twins under a unique-violation. Status starts pending;
	// the heartbeat reconciler promotes to ready on first heartbeat.
	node, err := q.CreateNode(ctx, store.CreateNodeParams{
		ID:                      uuid.New(),
		Name:                    req.NodeName,
		Architecture:            store.CPUArch(req.Architecture),
		AdvertisedEndpoint:      req.AdvertisedEndpoint,
		MigrationHost:           req.MigrationHost,
		MigrationPortRangeStart: req.MigrationPortRangeStart,
		MigrationPortRangeEnd:   req.MigrationPortRangeEnd,
		Status:                  store.NodeStatusPending,
	})
	if err != nil {
		return store.Node{}, fmt.Errorf("create node: %v", err)
	}
	return node, nil
}
