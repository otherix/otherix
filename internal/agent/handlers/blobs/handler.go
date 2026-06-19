// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package blobs hosts the agent's CP-driven /v1/blobs/* control handlers.
// The Control Plane dials these over the agent's main mTLS server
// (CP-only identity) to orchestrate a cross-node blob pull:
//
//   - POST /v1/blobs/serve tells a holder agent to open a peer-facing blob
//     serve listener and returns its reachable endpoint (synchronous 200).
//   - POST /v1/blobs/pull tells a consumer agent to fetch a blob from a holder
//     and lands it in the local artifact store (asynchronous 202, agent task).
//
// The blob bytes themselves flow consumer<-holder over the SEPARATE blobpeer
// listener (internal/agent/blobpeer), gated by the node leaf cert plus the
// per-op token; that data path is not handled here.
package blobs

import (
	"log/slog"

	"github.com/go-chi/chi/v5"
)

// BlobServer is the seam the serve handler drives: open a peer-facing serve
// listener for digest, authorized by the per-op token and scoped to
// consumerNodeID, and return the reachable serve endpoint plus its expiry
// (RFC 3339). Production wraps the blobpeer serve manager (server.go); tests
// pass a spy.
type BlobServer interface {
	Serve(digest, token, consumerNodeID string) (endpoint, expiresAt string, err error)
}

// BlobPuller is the seam the pull handler drives: start an agent task that
// streams the blob for digest from holderEndpoint (presenting token) into the
// local artifact store, and return the task id immediately. holderIdentity
// (when non-empty) pins TLS verification to the holder's node identity SAN.
// expectedSize (when > 0) bounds the streamed body to the CP-known blob size so
// a misbehaving holder cannot fill the disk before the digest check.
// Production wraps a blobpeer.Pull adapter (server.go); tests pass a spy.
type BlobPuller interface {
	Pull(digest, token, holderEndpoint, holderIdentity string, expectedSize int64) (taskID string, err error)
}

// BlobStopper is the seam the stop-serve handler drives: tear down the serve
// listener identified by its per-op token, releasing its reserved port. The
// token (not the digest) is the key so two concurrent serves of the same digest
// to different consumers stay disambiguated. Idempotent: a token with no live
// serve is a no-op. Production wraps the serve manager (server.go); tests pass a
// spy.
type BlobStopper interface {
	StopServe(token string)
}

// BlobDeleter is the seam the reclaim handler drives: delete one
// content-addressed blob (and its checksum sidecar) by digest from the local
// artifact store. Idempotent: deleting an absent blob is a no-op. Production
// wraps *artifactstore.Store; tests pass a spy.
type BlobDeleter interface {
	Delete(digest string) error
}

// Handler bundles the serve / pull / stop / delete seams and the logger. All
// state (the serve manager's listeners, the task store, the artifact store)
// lives behind the seams; the handler only translates wire shapes.
type Handler struct {
	server  BlobServer
	puller  BlobPuller
	stopper BlobStopper
	deleter BlobDeleter
	log     *slog.Logger
}

// New constructs a Handler over the serve / pull / stop / delete seams.
func New(s BlobServer, p BlobPuller, stop BlobStopper, del BlobDeleter, log *slog.Logger) *Handler {
	return &Handler{server: s, puller: p, stopper: stop, deleter: del, log: log}
}

// Mount registers /v1/blobs routes on r.
func (h *Handler) Mount(r chi.Router) {
	r.Post("/serve", h.Serve)
	r.Post("/pull", h.Pull)
	r.Post("/stop-serve", h.StopServe)
	r.Post("/reclaim", h.Reclaim)
}

// asyncAccepted mirrors `AsyncTaskAccepted` in api/openapi/agent.yaml.
// Duplicated from handlers/storagepools pending a shared types package; status
// enum is constrained to [pending, running] by spec.
type asyncAccepted struct {
	TaskID string         `json:"task_id"`
	Status string         `json:"status"`
	Links  map[string]any `json:"links"`
}
