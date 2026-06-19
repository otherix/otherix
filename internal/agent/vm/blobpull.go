// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/artifactstore"
	"github.com/otherix/otherix/internal/agent/blobpeer"
)

// ArtifactStore returns the node's content-addressed artifact store, or nil
// when none was configured (an empty cfg.Artifacts.Root - the test path). The
// agent runtime (server.go) reuses this single instance for the relocation
// sweep, the heartbeat blob inventory, and the blobpeer serve / pull handlers so
// every component shares one store.
func (m *Manager) ArtifactStore() *artifactstore.Store { return m.artifactStore }

// PoolRoots returns the filesystem root of every registered pool. The blob
// relocation sweep (server.go boot) walks these to move disk-pool-resident
// snapshot blobs into the artifact store.
func (m *Manager) PoolRoots() []string {
	m.poolsMu.RLock()
	defer m.poolsMu.RUnlock()
	out := make([]string, 0, len(m.pools))
	for _, p := range m.pools {
		out = append(out, p.root)
	}
	return out
}

// RelocateSnapshotsToStore runs the one-time, best-effort, fail-open sweep that
// moves disk-pool-resident snapshot blobs (and manifests) into the artifact
// store, over every registered pool root. A nil store (test path) is a no-op.
// Called once at agent boot by the runtime (server.go).
func (m *Manager) RelocateSnapshotsToStore() {
	relocateSnapshotsToStore(m.log, m.PoolRoots(), m.artifactStore)
}

// PullBlob starts a tracked agent task that streams the blob for digest from
// holderEndpoint (presenting the per-op token) into the node's artifact store
// over the supplied mTLS client, and returns the task immediately in `pending`
// status. The CP polls /v1/tasks/{id} for terminal status - the task lives in
// the same TaskStore the /v1/tasks handler reads. Returns ErrNoArtifactStore
// when the node has no artifact store configured (the blob has nowhere to land).
//
// The actual transfer runs in a goroutine: blobpeer.Pull re-verifies the digest
// while writing into the store (fail-closed - a wrong-bytes holder never
// materializes a blob), so a verification failure fails the task rather than
// landing a corrupt blob.
func (m *Manager) PullBlob(ctx context.Context, client *http.Client, digest, token, holderEndpoint, holderIdentity string, expectedSize int64) (*AgentTask, error) {
	if m.artifactStore == nil {
		return nil, ErrNoArtifactStore
	}
	task := m.tasks.Create(TaskKindBlobPull, uuid.Nil)
	go m.runPullBlob(context.WithoutCancel(ctx), task.ID, client, digest, token, holderEndpoint, holderIdentity, expectedSize)
	return task, nil
}

// runPullBlob is the goroutine body for PullBlob: transition to running, run the
// pull, and record terminal status. context.WithoutCancel keeps the transfer
// alive past the triggering HTTP request's context.
func (m *Manager) runPullBlob(ctx context.Context, taskID uuid.UUID, client *http.Client, digest, token, holderEndpoint, holderIdentity string, expectedSize int64) {
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	err := blobpeer.Pull(ctx, blobpeer.PullArgs{
		Endpoint:       holderEndpoint,
		Digest:         digest,
		Token:          token,
		Store:          m.artifactStore,
		TLSClient:      client,
		HolderIdentity: holderIdentity,
		ExpectedSize:   expectedSize,
	})
	if err != nil {
		m.log.Error("blob pull task failed",
			"task_id", taskID, "digest", digest, "holder_endpoint", holderEndpoint, "err", err)
		m.tasks.Update(taskID, func(t *AgentTask) {
			t.Status = TaskStatusFailed
			t.Error = &TaskError{Code: "blob_pull_failed", Message: err.Error()}
		})
		return
	}
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusSuccess })
}
