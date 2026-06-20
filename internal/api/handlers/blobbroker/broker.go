// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package blobbroker is the CP-side control that orchestrates a cross-node
// blob pull saga: discover the live holders from observed inventory, then for
// each in turn mint a single-use per-op token, tell the holder to serve, tell
// the consumer to pull and await it to terminal, and tear the serve down. It
// advances to the next live holder on a serve or pull failure and returns on the
// first success. It fails closed with ErrBlobUnavailable when no live holder
// exists - the honest availability boundary.
package blobbroker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
	"github.com/otherix/otherix/internal/api/agentclient"
	"github.com/otherix/otherix/internal/store"
)

// ErrBlobUnavailable is returned by BrokerPull when no node reports holding the
// requested blob in observed inventory. Callers (recreate-from-snapshot)
// branch on it via errors.Is to surface a fail-closed blob_unavailable.
var ErrBlobUnavailable = errors.New("blobbroker: no live holder for blob")

// Tier selects which content-addressed store a broker pull targets. Artifact is
// the durability-tracked store (snapshots, replication); Image is the node-level
// pinned-image cache tier. They use separate holder-discovery sources and the
// consumer writes the pulled blob into the matching store.
type Tier string

// The broker pull tiers: TierArtifact is the durability-tracked store, TierImage
// is the node-level pinned-image cache.
const (
	TierArtifact Tier = "artifact"
	TierImage    Tier = "image"
)

// pullTokenTTL bounds the lifetime of the single-use per-op token minted for one
// pull. The holder's serve listener self-expires at this TTL, which is
// also the broker's teardown backstop.
const pullTokenTTL = 5 * time.Minute

// Store is the CP store surface the broker needs: holder discovery from observed
// inventory, node endpoint resolution, saga create / phase mutation. The saga
// mutators are last-writer-wins; the broker mutates one saga sequentially from a
// single goroutine, so no CAS is required here (see etcdstore notes).
type Store interface {
	BlobHolders(ctx context.Context, digest string) ([]uuid.UUID, error)
	ImageBlobHolders(ctx context.Context, digest string) ([]uuid.UUID, error)
	BlobSize(ctx context.Context, digest string) (int64, bool)
	ImageBlobSize(ctx context.Context, digest string) (int64, bool)
	NodeByID(ctx context.Context, id uuid.UUID) (store.Node, error)
	CreatePullSaga(ctx context.Context, p store.CreatePullSagaParams) (store.ArtifactPullSaga, string, error)
	UpdatePullSagaServeEndpoint(ctx context.Context, id uuid.UUID, endpoint string) error
	SetPullSagaPhase(ctx context.Context, id uuid.UUID, phase store.PullSagaPhase) error
}

// AgentExecutor is the agent-facing seam: open the holder's serve listener, tell
// the consumer to pull and AWAIT it to terminal, and (best-effort) tear the
// serve listener down. PullBlobAndAwait blocks until the consumer's pull task
// reaches a terminal status; a non-success terminal (e.g. a verify failure on
// the agent) surfaces as a non-nil error so the broker fails the saga.
type AgentExecutor interface {
	// ServeBlob opens the holder's serve listener and returns the reachable
	// serve endpoint and its RFC 3339 expiry.
	ServeBlob(ctx context.Context, holderEndpoint, digest, token, consumerNodeID string) (serveEndpoint, expiresAt string, err error)
	// PullBlobAndAwait tells the consumer to pull from the holder's serve
	// endpoint and blocks until the consumer pull task reaches a terminal
	// status, returning an error on any non-success terminal. holderIdentity is
	// the holder's node identity SAN the consumer pins TLS verification against.
	// expectedSize (when > 0) is the CP-known blob size the consumer bounds the
	// pull body to; 0 means unknown (no cap, digest verify still enforced). tier
	// selects which content-addressed store the consumer writes the pulled blob
	// into.
	PullBlobAndAwait(ctx context.Context, consumerEndpoint, digest, token, holderEndpoint, holderIdentity string, expectedSize int64, tier Tier) error
	// StopServe tears the holder's serve listener down, keyed on the per-op
	// token the serve was minted with (NOT the digest: two concurrent serves of
	// the same digest to different consumers exist as two tokens, so a
	// digest-keyed teardown would be ambiguous). Best-effort: the listener also
	// self-expires on the token TTL if the CP never calls this.
	StopServe(ctx context.Context, holderEndpoint, token string) error
}

// Broker orchestrates one pull saga at a time via BrokerPull.
type Broker struct {
	store Store
	exec  AgentExecutor
	log   *slog.Logger
}

// New builds a Broker over the store and agent executor seams.
func New(st Store, exec AgentExecutor, log *slog.Logger) *Broker {
	return &Broker{store: st, exec: exec, log: log}
}

// BrokerPull makes the blob identified by digest present on consumerNodeID by
// pulling it from a live holder. It discovers the holders from observed
// inventory, filters them to the LIVE set (a holder reported unreachable or gone,
// or whose node lookup fails, is skipped) preserving the inventory order, and
// fails closed with ErrBlobUnavailable when the live set is empty (no holders, or
// all holders dead). It then iterates the live holders: for each it mints a
// single-use token, opens the holder serve listener, drives the consumer pull to
// terminal, and ALWAYS tears the serve listener down. It returns nil on the first
// holder that completes successfully; on a serve or pull failure it advances to
// the next live holder, and if every live holder fails it returns the last cause
// wrapped. The saga phase of the attempted holder mirrors its outcome.
//
// The consumer pull is awaited to terminal, and that await is bounded by BOTH
// the caller's ctx deadline AND the agent client's configured timeout
// (agent_client.timeout, default 5 minutes). The caller (the recreate
// worker) must therefore pass a ctx whose deadline accommodates a full blob
// transfer, and operators sizing very large blobs may need to raise
// agent_client.timeout; a transfer exceeding either bound surfaces as a timeout
// that fails the saga, not a broker bug.
func (b *Broker) BrokerPull(ctx context.Context, digest string, consumerNodeID uuid.UUID, tier Tier) error {
	var holders []uuid.UUID
	var err error
	switch tier {
	case TierImage:
		holders, err = b.store.ImageBlobHolders(ctx, digest)
	default:
		holders, err = b.store.BlobHolders(ctx, digest)
	}
	if err != nil {
		return fmt.Errorf("discover holders: %v", err)
	}

	consumer, err := b.store.NodeByID(ctx, consumerNodeID)
	if err != nil {
		return fmt.Errorf("load consumer node: %v", err)
	}

	// Filter holders to the LIVE set, preserving the inventory order so the choice
	// stays deterministic. A holder reported unreachable or gone, or one whose node
	// lookup fails, is skipped: BlobHolders prunes only gone nodes from inventory,
	// so an unreachable holder can still appear here and would otherwise drive a
	// serve at a dead endpoint.
	var live []store.Node
	for _, id := range holders {
		n, err := b.store.NodeByID(ctx, id)
		if err != nil {
			b.log.WarnContext(ctx, "blobbroker: skip holder, node lookup failed",
				"holder_id", id, "error", err)
			continue
		}
		if !isLive(n) {
			continue
		}
		live = append(live, n)
	}
	if len(live) == 0 {
		return ErrBlobUnavailable
	}

	// Iterate the live holders, advancing on failure and returning on the first
	// success. lastErr carries the most recent per-holder cause so an all-fail
	// outcome surfaces a real reason rather than the no-holder sentinel.
	var lastErr error
	for _, holder := range live {
		if err := b.pullFromHolder(ctx, digest, holder, consumer, consumerNodeID, tier); err != nil {
			b.log.WarnContext(ctx, "blobbroker: holder pull failed, trying next live holder",
				"holder", holder.AdvertisedEndpoint, "error", err)
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("all live holders failed: %v", lastErr)
}

// pullFromHolder runs one per-holder pull saga: mint a single-use token, open the
// holder serve listener, drive the consumer pull to terminal, then ALWAYS tear
// the holder serve listener down. The teardown is deferred so it runs on every
// return path (serve failure, pull failure, success) - no serve listener leaks
// when the caller falls through to the next holder. It returns nil only when the
// consumer pull completed successfully; the saga phase mirrors the outcome.
func (b *Broker) pullFromHolder(ctx context.Context, digest string, holder, consumer store.Node, consumerNodeID uuid.UUID, tier Tier) error {
	saga, token, err := b.store.CreatePullSaga(ctx, store.CreatePullSagaParams{
		ID:           uuid.New(),
		Digest:       digest,
		ConsumerNode: consumerNodeID,
		HolderNode:   holder.ID,
		TokenTTL:     pullTokenTTL,
	})
	if err != nil {
		return fmt.Errorf("create pull saga: %v", err)
	}

	// ServeBlob discards expiresAt: the broker does not act on the serve expiry. The
	// holder's serve listener self-expires on its token TTL (and is torn down by
	// the agent on shutdown), so the broker relies on that self-expiry rather
	// than tracking the deadline.
	serveEndpoint, _, err := b.exec.ServeBlob(ctx, holder.AdvertisedEndpoint, digest, token, consumerNodeID.String())
	if err != nil {
		// best-effort: saga phase is non-authoritative metadata; a write failure must not abort an in-progress pull.
		_ = b.store.SetPullSagaPhase(ctx, saga.ID, store.PullSagaPhaseFailed)
		return fmt.Errorf("holder serve: %v", err)
	}

	// Teardown is best-effort and ALWAYS runs (deferred so it fires on every
	// return path below, including the pull-failure fallthrough): a stop failure
	// logs but never fails a pull that otherwise succeeded (the listener
	// self-expires on TTL). Keyed on the per-op token (minted by CreatePullSaga),
	// not the digest, so it names exactly this serve even if another serve of the
	// same digest is live.
	defer func() {
		if stopErr := b.exec.StopServe(ctx, holder.AdvertisedEndpoint, token); stopErr != nil {
			b.log.WarnContext(ctx, "blobbroker: stop serve failed (best-effort)",
				"holder", holder.AdvertisedEndpoint, "error", stopErr)
		}
	}()

	// Records the serve endpoint and advances the saga to serving.
	// best-effort: saga endpoint is non-authoritative metadata; a write failure must not abort an in-progress pull.
	_ = b.store.UpdatePullSagaServeEndpoint(ctx, saga.ID, serveEndpoint)

	// best-effort: saga phase is non-authoritative metadata; a write failure must not abort an in-progress pull.
	_ = b.store.SetPullSagaPhase(ctx, saga.ID, store.PullSagaPhasePulling)
	// Pin the consumer's TLS verification of the holder to the holder's node
	// identity SAN, not the dialed inter-node IP (the holder's node leaf cert
	// carries the identity, not the IP). Mirrors migration's target_node_identity.
	hid := holderIdentity(holder.Name)
	// Resolve the CP-known blob size (any holder's reported size is authoritative
	// for a content-addressed blob) so the consumer can bound the pull body. The
	// size must come from the SAME tier the pull targets: BlobSize reads
	// node_blobs (the artifact tier) and returns 0 for an image-only digest, which
	// would leave an image-tier pull uncapped. A zero/absent size means unknown -
	// the consumer then falls back to an absolute cap and relies on the digest
	// verify.
	var expectedSize int64
	if tier == TierImage {
		expectedSize, _ = b.store.ImageBlobSize(ctx, digest)
	} else {
		expectedSize, _ = b.store.BlobSize(ctx, digest)
	}
	if pullErr := b.exec.PullBlobAndAwait(ctx, consumer.AdvertisedEndpoint, digest, token, serveEndpoint, hid, expectedSize, tier); pullErr != nil {
		// best-effort: saga phase is non-authoritative metadata; a write failure must not abort an in-progress pull.
		_ = b.store.SetPullSagaPhase(ctx, saga.ID, store.PullSagaPhaseFailed)
		return fmt.Errorf("consumer pull: %v", pullErr)
	}
	return b.store.SetPullSagaPhase(ctx, saga.ID, store.PullSagaPhaseComplete)
}

// isLive reports whether a node may serve a blob pull: not soft-deleted and not
// in a terminal-or-stale status (unreachable or gone). It mirrors the durability
// reconciler's liveness predicate so holder selection and replica placement agree
// on what "live" means.
func isLive(n store.Node) bool {
	return n.DeletedAt == nil &&
		n.Status != store.NodeStatusUnreachable &&
		n.Status != store.NodeStatusGone
}

// holderIdentity is the holder node's SAN name the consumer pins TLS
// verification against ("node-<name>.agents.otherix.local"). It mirrors the
// migration target_node_identity helper (internal/auth/csr.go SignCSR puts this
// SAN on every node leaf cert), so the consumer can dial the holder by its
// inter-node IP yet verify the cert against the identity the cert actually
// carries.
func holderIdentity(nodeName string) string { return "node-" + nodeName + ".agents.otherix.local" }

// ClientExecutor adapts *agentclient.Client to the AgentExecutor seam. It is the
// production wiring used by the worker that drives BrokerPull; the
// broker orchestration itself is unit-tested against a spy.
type ClientExecutor struct {
	c *agentclient.Client
}

// NewClientExecutor builds the production AgentExecutor over an agentclient.
func NewClientExecutor(c *agentclient.Client) *ClientExecutor {
	return &ClientExecutor{c: c}
}

// ServeBlob calls agentclient.ServeBlob and projects the response to the seam.
func (e *ClientExecutor) ServeBlob(ctx context.Context, holderEndpoint, digest, token, consumerNodeID string) (string, string, error) {
	cid, err := uuid.Parse(consumerNodeID)
	if err != nil {
		return "", "", fmt.Errorf("parse consumer node id: %v", err)
	}
	resp, err := e.c.ServeBlob(ctx, holderEndpoint, agentapi.BlobServeRequest{
		ConsumerNodeID: cid,
		Digest:         digest,
		Token:          token,
	})
	if err != nil {
		return "", "", err
	}
	return resp.ServeEndpoint, resp.ExpiresAt.Format(time.RFC3339), nil
}

// PullBlobAndAwait tells the consumer to pull (async 202 + task id), then polls
// the consumer agent's task to terminal, mirroring the migration await. A
// non-success terminal (a verify failure on the agent surfaces as a failed task)
// returns an error so the broker fails the saga.
func (e *ClientExecutor) PullBlobAndAwait(ctx context.Context, consumerEndpoint, digest, token, holderEndpoint, holderIdentity string, expectedSize int64, tier Tier) error {
	req := agentapi.BlobPullRequest{
		Digest:         digest,
		HolderEndpoint: holderEndpoint,
		Token:          token,
	}
	if holderIdentity != "" {
		req.HolderIdentity = &holderIdentity
	}
	if expectedSize > 0 {
		req.ExpectedSize = &expectedSize
	}
	// Image tier writes the pulled blob into the node-level image cache; artifact
	// (the default) keeps the omitted wire shape so existing pulls are unchanged.
	if tier == TierImage {
		t := agentapi.BlobPullRequestTierImage
		req.Tier = &t
	}
	taskID, err := e.c.PullBlob(ctx, consumerEndpoint, req)
	if err != nil {
		return err
	}
	tid, err := uuid.Parse(taskID)
	if err != nil {
		return fmt.Errorf("parse pull task id: %v", err)
	}
	terminal, err := e.c.PollTask(ctx, consumerEndpoint, tid)
	if err != nil {
		return fmt.Errorf("poll pull task: %v", err)
	}
	if terminal.Status != "success" {
		if terminal.Error != nil {
			return fmt.Errorf("pull task %s: %v", terminal.Status, terminal.Error)
		}
		return fmt.Errorf("pull task %s", terminal.Status)
	}
	return nil
}

// StopServe tells the holder agent to tear down the serve listener identified by
// its per-op token, releasing the reserved port at pull completion instead of
// waiting out the token TTL. The holder treats it as idempotent (a token with no
// live serve is a no-op), and the broker calls it best-effort: the listener
// self-expires on the token TTL should this call (or the CP) never land.
func (e *ClientExecutor) StopServe(ctx context.Context, holderEndpoint, token string) error {
	return e.c.StopServe(ctx, holderEndpoint, agentapi.BlobStopServeRequest{Token: token})
}
