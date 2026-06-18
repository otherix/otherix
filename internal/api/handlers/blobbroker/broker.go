// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package blobbroker is the CP-side control that orchestrates a cross-node
// blob pull saga (slice C1): discover a live holder from observed inventory,
// mint a single-use per-op token, tell the holder to serve, tell the consumer
// to pull and await it to terminal, then tear down. It fails closed with
// ErrBlobUnavailable when no holder exists - the honest K=1 boundary.
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
// requested blob in observed inventory. Callers (Task 13 recreate-from-snapshot)
// branch on it via errors.Is to surface a fail-closed blob_unavailable.
var ErrBlobUnavailable = errors.New("blobbroker: no live holder for blob")

// pullTokenTTL bounds the lifetime of the single-use per-op token minted for one
// pull. The holder's serve listener self-expires at this TTL (Task 10), which is
// also the broker's teardown backstop.
const pullTokenTTL = 5 * time.Minute

// Store is the CP store surface the broker needs: holder discovery from observed
// inventory, node endpoint resolution, saga create / phase mutation. The saga
// mutators are last-writer-wins; the broker mutates one saga sequentially from a
// single goroutine, so no CAS is required here (see etcdstore notes).
type Store interface {
	BlobHolders(ctx context.Context, digest string) ([]uuid.UUID, error)
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
	// status, returning an error on any non-success terminal.
	PullBlobAndAwait(ctx context.Context, consumerEndpoint, digest, token, holderEndpoint string) error
	// StopServe tears the holder's serve listener down. Best-effort: the
	// listener also self-expires on the token TTL.
	StopServe(ctx context.Context, holderEndpoint, digest string) error
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
// pulling it from a live holder. It discovers a holder from observed inventory
// (fail-closed ErrBlobUnavailable when none), mints a single-use token, opens
// the holder serve listener, drives the consumer pull to terminal, then tears
// the serve listener down. Returns nil only when the consumer pull completed
// successfully; the saga phase mirrors the outcome.
func (b *Broker) BrokerPull(ctx context.Context, digest string, consumerNodeID uuid.UUID) error {
	holders, err := b.store.BlobHolders(ctx, digest)
	if err != nil {
		return fmt.Errorf("discover holders: %v", err)
	}
	if len(holders) == 0 {
		return ErrBlobUnavailable
	}
	holderID := holders[0] // C1: any live holder; placement policy is C2.

	holder, err := b.store.NodeByID(ctx, holderID)
	if err != nil {
		return fmt.Errorf("load holder node: %v", err)
	}
	consumer, err := b.store.NodeByID(ctx, consumerNodeID)
	if err != nil {
		return fmt.Errorf("load consumer node: %v", err)
	}

	saga, token, err := b.store.CreatePullSaga(ctx, store.CreatePullSagaParams{
		ID:           uuid.New(),
		Digest:       digest,
		ConsumerNode: consumerNodeID,
		HolderNode:   holderID,
		TokenTTL:     pullTokenTTL,
	})
	if err != nil {
		return fmt.Errorf("create pull saga: %v", err)
	}

	serveEndpoint, _, err := b.exec.ServeBlob(ctx, holder.AdvertisedEndpoint, digest, token, consumerNodeID.String())
	if err != nil {
		_ = b.store.SetPullSagaPhase(ctx, saga.ID, store.PullSagaPhaseFailed)
		return fmt.Errorf("holder serve: %v", err)
	}
	// Records the serve endpoint and advances the saga to serving.
	_ = b.store.UpdatePullSagaServeEndpoint(ctx, saga.ID, serveEndpoint)

	_ = b.store.SetPullSagaPhase(ctx, saga.ID, store.PullSagaPhasePulling)
	pullErr := b.exec.PullBlobAndAwait(ctx, consumer.AdvertisedEndpoint, digest, token, serveEndpoint)

	// Teardown is best-effort and ALWAYS runs: a stop failure logs but never
	// fails a pull that otherwise succeeded (the listener self-expires on TTL).
	if stopErr := b.exec.StopServe(ctx, holder.AdvertisedEndpoint, digest); stopErr != nil {
		b.log.WarnContext(ctx, "blobbroker: stop serve failed (best-effort)",
			"holder", holder.AdvertisedEndpoint, "error", stopErr)
	}

	if pullErr != nil {
		_ = b.store.SetPullSagaPhase(ctx, saga.ID, store.PullSagaPhaseFailed)
		return fmt.Errorf("consumer pull: %v", pullErr)
	}
	return b.store.SetPullSagaPhase(ctx, saga.ID, store.PullSagaPhaseComplete)
}

// ClientExecutor adapts *agentclient.Client to the AgentExecutor seam. It is the
// production wiring used by the worker that drives BrokerPull (Task 13); the
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
func (e *ClientExecutor) PullBlobAndAwait(ctx context.Context, consumerEndpoint, digest, token, holderEndpoint string) error {
	taskID, err := e.c.PullBlob(ctx, consumerEndpoint, agentapi.BlobPullRequest{
		Digest:         digest,
		HolderEndpoint: holderEndpoint,
		Token:          token,
	})
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

// StopServe is a no-op in C1: the Task-10 holder serve listener self-expires on
// the token TTL, so there is no stop-serve agent endpoint. The method is kept as
// part of the seam (and exercised in order by the broker) for symmetry and so a
// future explicit teardown can be wired without touching the broker.
func (e *ClientExecutor) StopServe(_ context.Context, _, _ string) error { return nil }
