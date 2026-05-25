// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agentmock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agentapi"
)

// StorageImagesImport implements POST /v1/storage-pools/{pool_id}/images
// per agent.yaml. The handler decodes the ImageImportRequest body,
// mints a fresh agent-task uuid, registers an `agentTask` carrying
// the import-flavoured outcome, and returns 202 + AsyncTaskAccepted.
// The terminal projection materialises lazily on later
// GET /v1/tasks/{id} calls — see projectAgentTask and (for the side
// effect on state.images) materializeImportLocked.
//
// Outcome selection:
//
//  1. If the requested checksum is already staged in the pool,
//     terminal-success uses the existing entry's size / format and
//     does NOT consume the AddImageImportResult queue. This mirrors
//     the agent's documented idempotent re-import contract.
//  2. Otherwise the next queued ImageImportResult drives the
//     outcome; an empty queue yields a default 1 MiB qcow2 success.
func (m *Mock) StorageImagesImport(
	w http.ResponseWriter,
	r *http.Request,
	poolName agentapi.PoolName,
	_ agentapi.StorageImagesImportParams,
) {
	const opID = "storageImages.import"
	if m.preDispatch(w, r, opID) {
		return
	}

	var body agentapi.ImageImportRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		m.respondJSON(w, r, opID, http.StatusBadRequest, errorEnvelope{
			Error: errorEnvelopeBody{
				Code:    "validation_failed",
				Message: "decode ImageImportRequest: " + err.Error(),
			},
		})
		return
	}

	agentTaskID := uuid.New()

	m.state.mu.Lock()
	p, ok := m.state.pools[poolName]
	if !ok {
		m.state.mu.Unlock()
		m.respondNotFound(w, r, opID, "storage pool not found on this agent")
		return
	}
	poolID := p.ID

	// `expected_checksum_sha256` is *string + omitempty after the
	// agent.yaml relaxation. Two trust modes:
	//
	//   - Verify mode: pointer non-nil и value is а 64-char lowercase
	//     hex. The mock keys the pre-existence shortcut и the FIFO
	//     queue lookup on this value, и echoes it back as the result's
	//     `checksum_sha256` so the wire shape matches а real agent's
	//     verify-mode terminal projection.
	//   - Compute mode: pointer nil или empty. No pre-existence
	//     shortcut (the agent would not know the filename). The mock
	//     consumes the empty-checksum FIFO queue slot, then surfaces
	//     either the queued ComputedChecksumSHA256 (when set) or а
	//     deterministic synthetic checksum from sha256(pool || source).
	expectedChecksum := ""
	if body.ExpectedChecksumSha256 != nil {
		expectedChecksum = *body.ExpectedChecksumSha256
	}
	verifyMode := expectedChecksum != ""

	var outcome ImageImportResult
	switch {
	case verifyMode:
		if existing, present := m.state.images[poolName][expectedChecksum]; present {
			outcome = ImageImportResult{
				Status:    "success",
				SizeBytes: existing.SizeBytes,
				Format:    existing.Format,
				Delay:     defaultPoolScanDelay,
			}
		} else {
			outcome = m.state.takeImageImportResultLocked(poolName, expectedChecksum)
		}
	default:
		// Compute mode — queue keyed by the empty string. Test authors
		// drive this via AddImageImportResult(pool, "", outcome).
		outcome = m.state.takeImageImportResultLocked(poolName, "")
	}

	// Resolve the checksum the mock surfaces в task.result.
	// Verify mode echoes the request's expected value; compute mode
	// uses the queued ComputedChecksumSHA256 OR а deterministic
	// fallback so tests that don't care about the value still get
	// а valid 64-char lowercase hex.
	importChecksum := expectedChecksum
	if !verifyMode {
		importChecksum = outcome.ComputedChecksumSHA256
		if importChecksum == "" {
			importChecksum = computeFallbackChecksum(poolID, body)
		}
	}

	m.state.tasks[agentTaskID] = &agentTask{
		id:             agentTaskID,
		taskType:       "storage_image.import",
		resourceType:   "storage_pool",
		resourceID:     poolID,
		poolName:       poolName,
		createdAt:      time.Now().UTC(),
		delay:          outcome.Delay,
		importResult:   &outcome,
		importChecksum: importChecksum,
	}
	m.state.mu.Unlock()

	m.respondJSON(w, r, opID, http.StatusAccepted, agentapi.AsyncTaskAccepted{
		TaskID: agentTaskID,
		Status: agentapi.AsyncTaskAcceptedStatus("pending"),
		Links: struct {
			Self string `json:"self"`
		}{Self: "/v1/tasks/" + agentTaskID.String()},
	})
}

// computeFallbackChecksum produces а deterministic stand-in sha256 hex
// for compute-mode requests that don't carry а queued
// ComputedChecksumSHA256. Derives the digest from the pool uuid и the
// request's source identifier (URL or path) so repeated calls в the
// same test scenario land at the same `templates/{sha}.qcow2` path,
// preserving the storage-layout invariant the real agent enforces.
func computeFallbackChecksum(poolID uuid.UUID, body agentapi.ImageImportRequest) string {
	h := sha256.New()
	h.Write(poolID[:])
	if body.SourceURL != nil {
		_, _ = h.Write([]byte(*body.SourceURL))
	}
	if body.SourcePath != nil {
		_, _ = h.Write([]byte(*body.SourcePath))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// StorageImagesDelete implements DELETE
// /v1/storage-pools/{pool_id}/images/{checksum}. Synchronous,
// idempotent — agent contract returns 204 whether or not the file
// was present (a real agent's `rm -f`). The CP-side
// agentclient.DeleteImage already collapses 204 / 404 to nil; we
// keep the mock honest to the contract by emitting 204 in both
// cases rather than leaking an internal "missing" branch through
// the wire.
//
// Pool-not-registered also returns 204 (the test-API workflow does
// not register pools dynamically — operators that need
// pool-existence error semantics should use the explicit
// InjectError("storageImages.delete", ...) escape hatch).
func (m *Mock) StorageImagesDelete(
	w http.ResponseWriter,
	r *http.Request,
	poolName agentapi.PoolName,
	checksum string,
	_ agentapi.StorageImagesDeleteParams,
) {
	const opID = "storageImages.delete"
	if m.preDispatch(w, r, opID) {
		return
	}

	m.state.mu.Lock()
	if bucket, ok := m.state.images[poolName]; ok {
		delete(bucket, checksum)
	}
	m.state.mu.Unlock()

	w.WriteHeader(http.StatusNoContent)
	m.recordRequest(opID, r, http.StatusNoContent)
}

// materializeImportLocked is the storage_image.import side effect
// the TasksGet handler runs alongside the projection: when the task
// has reached terminal-success and the corresponding (pool, sha256)
// entry is not yet staged in state.images, the handler stages it
// now using the import outcome's size / format. Tests observe the
// materialisation through ListStoredImages, StoredImage, or a
// follow-up StorageImagesList call.
//
// Caller holds m.state.mu. The function operates on the live
// state, not on the snapshot copy passed to projectAgentTask.
func (m *Mock) materializeImportLocked(snapshot *agentTask, now time.Time) {
	if snapshot.taskType != "storage_image.import" || snapshot.importResult == nil {
		return
	}
	if snapshot.importResult.Status != "success" {
		return
	}
	if now.Sub(snapshot.createdAt) < snapshot.delay {
		return
	}
	bucket, ok := m.state.images[snapshot.poolName]
	if !ok {
		// Pool was un-registered between import-time and projection.
		// Drop the materialisation silently — the wire response
		// remains terminal-success per the import outcome.
		return
	}
	if _, present := bucket[snapshot.importChecksum]; present {
		return
	}
	bucket[snapshot.importChecksum] = CachedImage{
		ChecksumSHA256: snapshot.importChecksum,
		Format:         snapshot.importResult.Format,
		SizeBytes:      snapshot.importResult.SizeBytes,
		Path:           "/opt/otherix/pools/" + snapshot.poolName + "/" + snapshot.importChecksum + "." + snapshot.importResult.Format,
	}
}
