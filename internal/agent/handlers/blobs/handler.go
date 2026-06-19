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
// Production wraps a blobpeer.Pull adapter (server.go); tests pass a spy.
type BlobPuller interface {
	Pull(digest, token, holderEndpoint, holderIdentity string) (taskID string, err error)
}

// Handler bundles the serve / pull seams and the logger. All state (the serve
// manager's listeners, the task store) lives behind the seams; the handler only
// translates wire shapes.
type Handler struct {
	server BlobServer
	puller BlobPuller
	log    *slog.Logger
}

// New constructs a Handler over the serve / pull seams.
func New(s BlobServer, p BlobPuller, log *slog.Logger) *Handler {
	return &Handler{server: s, puller: p, log: log}
}

// Mount registers /v1/blobs routes on r.
func (h *Handler) Mount(r chi.Router) {
	r.Post("/serve", h.Serve)
	r.Post("/pull", h.Pull)
}

// asyncAccepted mirrors `AsyncTaskAccepted` in api/openapi/agent.yaml.
// Duplicated from handlers/storagepools pending a shared types package; status
// enum is constrained to [pending, running] by spec.
type asyncAccepted struct {
	TaskID string         `json:"task_id"`
	Status string         `json:"status"`
	Links  map[string]any `json:"links"`
}
