// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/qemu"
	"github.com/otherix/otherix/internal/agentapi"
)

// snapshotsSubdir is the per-pool directory holding content-addressed snapshot
// disk blobs ({sha}.qcow2 + sidecar) plus the .staging and manifests subdirs.
const snapshotsSubdir = "snapshots"

// snapshotsStagingSubdir is the per-pool scratch dir under snapshots/ where a
// blob is materialized + hashed before its content-addressed name is known.
// The final atomic rename lands the blob beside it under the same snapshots/
// dir, so the rename is intra-directory (same filesystem, truly atomic).
const snapshotsStagingSubdir = ".staging"

// snapshotManifestsSubdir holds one JSON manifest per materialized snapshot,
// keyed by snapshot name ({snapshots}/manifests/{name}.json). The manifest is
// the agent-local record of which content-addressed blobs a snapshot
// references; it gates the (vm, name) idempotency check and the fail-closed
// blob GC on delete.
const snapshotManifestsSubdir = "manifests"

// SnapshotSpec is the post-validation create request the manager acts on.
// Slice A is disk-only: WithMemory is rejected (RAM capture is out of scope).
type SnapshotSpec struct {
	Name        string
	Description string
	WithMemory  bool
}

// ErrSnapshotWithMemory is returned by CreateSnapshot when with_memory is set.
// Slice A captures disks only; the CP already rejects this, so reaching the
// agent is defense in depth. Handlers map it to 400 validation_failed.
var ErrSnapshotWithMemory = errors.New("snapshot with_memory is unsupported (disk-only)")

// diskCapturer captures one running/stopped VM disk device into a destination
// backup file. The production impl dials the VM's QMP socket and runs
// blockdev-backup; tests inject a fake that copies a fixture qcow2. It is the
// seam that keeps CreateSnapshot unit-testable on darwin without real qemu.
type diskCapturer interface {
	Capture(ctx context.Context, v *VM, device, dest string) error
}

// qmpDiskCapturer is the production diskCapturer: it dials the VM's QMP socket
// and runs a full-copy blockdev-backup of device into dest (a fresh qcow2 the
// caller pre-creates is NOT required - BackupDiskToFile's blockdev-add opens
// the file, so the caller must create it first). It is crash-consistent and
// disk-only.
type qmpDiskCapturer struct{}

// Capture backs up device into dest via QMP. The dest qcow2 must already exist
// (qemu blockdev-add opens, does not create); the caller pre-creates it.
func (qmpDiskCapturer) Capture(ctx context.Context, v *VM, device, dest string) error {
	client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial qmp: %v", err)
	}
	defer func() { _ = client.Close() }()
	jobID := "snap-" + uuid.NewString()
	nodeName := "snaptgt-" + uuid.NewString()
	if err := client.BackupDiskToFile(ctx, jobID, nodeName, device, dest); err != nil {
		return fmt.Errorf("backup disk %s: %v", device, err)
	}
	return nil
}

// snapshotDiskDevice is one VM disk to capture: its virtio index, the wire
// device name (virtio<i>), and the on-disk source path. Slice A captures the
// boot disk only (index 0); the slice keeps the index-ordered shape so a
// future multi-disk VM maps blobs to disks deterministically (the same
// virtio<i> invariant the migration data-path relies on).
type snapshotDiskDevice struct {
	index  int
	device string
	src    string
}

// snapshotDiskDevices enumerates v's disks in virtio-index order. Slice A VMs
// carry a single boot disk (DiskPath) at index 0, device virtio0 - matching
// the migration path's source disk node naming. cidata (a read-only,
// deterministic ISO) is intentionally NOT captured; it is rebuilt on recreate.
func snapshotDiskDevices(v *VM) []snapshotDiskDevice {
	return []snapshotDiskDevice{{index: 0, device: "virtio0", src: v.DiskPath}}
}

// snapshotManifest is the agent-local JSON record of one materialized
// snapshot: which content-addressed blobs it references plus the metadata the
// CP worker's decodeSnapshotResult consumes. It both gates (vm, name)
// idempotency and drives the fail-closed blob GC on delete.
type snapshotManifest struct {
	Name              string                 `json:"name"`
	Description       string                 `json:"description,omitempty"`
	VMUUID            uuid.UUID              `json:"vm_uuid"`
	VMName            string                 `json:"vm_name"`
	Architecture      string                 `json:"architecture"`
	VMStateAtSnapshot string                 `json:"vm_state_at_snapshot"`
	CreatedAt         time.Time              `json:"created_at"`
	Disks             []snapshotManifestDisk `json:"disks"`
}

// snapshotManifestDisk is one disk entry in a snapshot manifest, ordered by
// index, device virtio<i>, with the content-addressed blob digest + size.
type snapshotManifestDisk struct {
	Index     int    `json:"index"`
	Device    string `json:"device"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Format    string `json:"format"`
}

// toAPI projects the manifest onto the agentapi.Snapshot wire shape the CP
// worker's agent_executor decodes (re-marshals the task result map and
// unmarshals into agentapi.Snapshot). Disks are ordered by index.
func (man snapshotManifest) toAPI() agentapi.Snapshot {
	disks := make([]struct {
		Device    string                       `json:"device"`
		Format    agentapi.SnapshotDisksFormat `json:"format"`
		Index     int                          `json:"index"`
		Sha256    string                       `json:"sha256"`
		SizeBytes int64                        `json:"size_bytes"`
	}, 0, len(man.Disks))
	for _, d := range man.Disks {
		disks = append(disks, struct {
			Device    string                       `json:"device"`
			Format    agentapi.SnapshotDisksFormat `json:"format"`
			Index     int                          `json:"index"`
			Sha256    string                       `json:"sha256"`
			SizeBytes int64                        `json:"size_bytes"`
		}{
			Device:    d.Device,
			Format:    agentapi.SnapshotDisksFormat(d.Format),
			Index:     d.Index,
			Sha256:    d.SHA256,
			SizeBytes: d.SizeBytes,
		})
	}
	out := agentapi.Snapshot{
		Architecture:      agentapi.SnapshotArchitecture(man.Architecture),
		CreatedAt:         man.CreatedAt,
		Disks:             disks,
		Name:              man.Name,
		Status:            agentapi.SnapshotStatusReady,
		VMStateAtSnapshot: agentapi.SnapshotVMStateAtSnapshot(man.VMStateAtSnapshot),
		VMUUID:            man.VMUUID,
	}
	if man.Description != "" {
		desc := man.Description
		out.Description = &desc
	}
	return out
}

// BlobResult describes one produced content-addressed snapshot disk blob:
// its sha256 digest (the content address), the final on-disk path
// ({snapshotsDir}/{sha}.qcow2), and its byte size.
type BlobResult struct {
	SHA256    string
	Path      string
	SizeBytes int64
}

// SnapshotBlob is the agent-internal view of one cached snapshot blob,
// projected by ListSnapshots from a filesystem walk of {poolRoot}/snapshots/.
type SnapshotBlob struct {
	SHA256    string
	Path      string
	SizeBytes int64
}

// convertFunc materializes a standalone qcow2 copy of src at dst. Production
// passes qemu.ConvertTo; tests pass a verbatim copy. It is the seam that
// keeps produceBlob's durability-critical hash/rename/sidecar tail unit
// testable without shelling out to qemu-img.
type convertFunc func(ctx context.Context, src, dst string) error

// produceBlob materializes srcDisk (a backup-target qcow2) into a
// content-addressed blob under snapshotsDir, mirroring the image cache's
// download-into-cache tail with a local convert source instead of HTTP:
//
//  1. convert srcDisk into {snapshotsDir}/.staging/<rand>.qcow2 (fresh,
//     standalone qcow2);
//  2. hash the staging file and validate its qcow2 magic;
//  3. if {snapshotsDir}/{sha}.qcow2 already exists with a matching sidecar,
//     reuse it (idempotent; drop the staging file);
//  4. otherwise atomically rename the staging file to {snapshotsDir}/{sha}.qcow2
//     then write the {sha}.qcow2.sha256 sidecar.
//
// The hash is computed BEFORE the final name is known and the blob is renamed
// into place only after it validates, so a crash never leaves a partial
// {sha}.qcow2: the staging file is the only thing that can be left behind, and
// it is cleaned up on the way out.
func produceBlob(ctx context.Context, srcDisk, snapshotsDir string, convert convertFunc) (BlobResult, error) {
	stagingDir := filepath.Join(snapshotsDir, snapshotsStagingSubdir)
	if err := os.MkdirAll(stagingDir, 0o750); err != nil {
		return BlobResult{}, fmt.Errorf("create snapshots staging dir: %v", err)
	}
	tempPath := filepath.Join(stagingDir, uuid.NewString()+".qcow2")
	defer func() { _ = os.Remove(tempPath) }()

	if err := convert(ctx, srcDisk, tempPath); err != nil {
		return BlobResult{}, fmt.Errorf("convert %s into staging blob: %v", srcDisk, err)
	}
	if err := validateQcow2Magic(tempPath); err != nil {
		return BlobResult{}, fmt.Errorf("qcow2_header_invalid: %v", err)
	}
	sha, err := hashFile(tempPath)
	if err != nil {
		return BlobResult{}, fmt.Errorf("hash staging blob: %v", err)
	}
	info, err := os.Stat(tempPath)
	if err != nil {
		return BlobResult{}, fmt.Errorf("stat staging blob: %v", err)
	}
	size := info.Size()

	blobPath := filepath.Join(snapshotsDir, sha+".qcow2")
	sidecarPath := blobPath + ".sha256"

	// Idempotent reuse: a prior identical blob already present (with a
	// well-formed matching sidecar) is the same content - drop the staging
	// copy and reuse it.
	if cachedSHA, cachedSize, present := readCachedImage(blobPath, sidecarPath); present && cachedSHA == sha {
		return BlobResult{SHA256: sha, Path: blobPath, SizeBytes: cachedSize}, nil
	}

	if err := os.Rename(tempPath, blobPath); err != nil {
		return BlobResult{}, fmt.Errorf("atomic rename to snapshots: %v", err)
	}
	if err := os.WriteFile(sidecarPath, []byte(sha), 0o644); err != nil { //nolint:gosec // sidecar is non-secret metadata
		return BlobResult{}, fmt.Errorf("write snapshot sidecar: %v", err)
	}
	return BlobResult{SHA256: sha, Path: blobPath, SizeBytes: size}, nil
}

// ListSnapshots walks snapshotsDir and returns the inventory of content-
// addressed snapshot blobs: every {sha}.qcow2 file paired with the digest
// read from its sidecar. Mirrors ListImages: files lacking a well-formed
// sidecar are skipped (partial produce or scratch), the .staging subdir is
// skipped, and an absent snapshotsDir yields an empty inventory and nil error.
func ListSnapshots(snapshotsDir string) ([]SnapshotBlob, error) {
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read snapshots dir: %w", err)
	}

	blobs := make([]SnapshotBlob, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".sha256") {
			continue
		}
		blobPath := filepath.Join(snapshotsDir, name)
		sha, size, present := readCachedImage(blobPath, blobPath+".sha256")
		if !present {
			continue
		}
		blobs = append(blobs, SnapshotBlob{
			SHA256:    sha,
			Path:      blobPath,
			SizeBytes: size,
		})
	}
	sort.Slice(blobs, func(i, j int) bool {
		return blobs[i].SHA256 < blobs[j].SHA256
	})
	return blobs, nil
}

// snapshotManifestPath returns the on-disk manifest path for (pool root,
// snapshot name): {root}/snapshots/manifests/{name}.json.
func snapshotManifestPath(poolRoot, name string) string {
	return filepath.Join(poolRoot, snapshotsSubdir, snapshotManifestsSubdir, name+".json")
}

// readSnapshotManifest loads the manifest at path. Returns (man, false, nil)
// when absent (not an error - the absence is the idempotency signal).
func readSnapshotManifest(path string) (snapshotManifest, bool, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // agent-owned pool path
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return snapshotManifest{}, false, nil
		}
		return snapshotManifest{}, false, fmt.Errorf("read snapshot manifest: %v", err)
	}
	var man snapshotManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return snapshotManifest{}, false, fmt.Errorf("decode snapshot manifest: %v", err)
	}
	return man, true, nil
}

// writeSnapshotManifest atomically writes man to its manifests/ path: a temp
// file in the same directory followed by an intra-directory rename, so a crash
// never leaves a half-written manifest the idempotency check would misread.
func writeSnapshotManifest(poolRoot string, man snapshotManifest) error {
	dir := filepath.Join(poolRoot, snapshotsSubdir, snapshotManifestsSubdir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create manifests dir: %v", err)
	}
	raw, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return fmt.Errorf("encode snapshot manifest: %v", err)
	}
	tmp := filepath.Join(dir, "."+uuid.NewString()+".json.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil { //nolint:gosec // manifest is non-secret metadata
		return fmt.Errorf("write staging manifest: %v", err)
	}
	if err := os.Rename(tmp, snapshotManifestPath(poolRoot, man.Name)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("atomic rename manifest: %v", err)
	}
	return nil
}

// listManifests reads every manifest under {poolRoot}/snapshots/manifests/.
// Files that fail to parse are skipped (best-effort); an absent dir yields an
// empty slice. Used by ListVMSnapshots and the fail-closed delete GC.
func listManifests(poolRoot string) ([]snapshotManifest, error) {
	dir := filepath.Join(poolRoot, snapshotsSubdir, snapshotManifestsSubdir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read manifests dir: %v", err)
	}
	out := make([]snapshotManifest, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		man, present, err := readSnapshotManifest(filepath.Join(dir, e.Name()))
		if err != nil || !present {
			continue
		}
		out = append(out, man)
	}
	return out, nil
}

// CreateSnapshot begins an async, disk-only snapshot capture for the named VM
// and returns the agent task tracking it. WithMemory is rejected (slice A is
// disk-only). Idempotency is (vm, snapshot_name)-keyed defense in depth: a
// redelivered create whose manifest already exists OR whose capture is still
// in flight returns the existing task/manifest and NEVER starts a second
// blockdev-backup - closing the CP-side worker-redelivery residual window.
func (m *Manager) CreateSnapshot(ctx context.Context, vmName string, spec SnapshotSpec) (*AgentTask, error) {
	if spec.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidSpec)
	}
	if spec.WithMemory {
		return nil, ErrSnapshotWithMemory
	}

	v, err := m.ByName(vmName)
	if err != nil {
		return nil, err
	}
	poolRoot, err := m.poolRoot(v.PoolName)
	if err != nil {
		return nil, err
	}

	key := snapshotInFlightKey{vmName: vmName, snapshotName: spec.Name}

	// Single critical section over both idempotency gates so two concurrent
	// first attempts cannot both launch a capture: (1) an in-flight capture
	// for the same key returns the original task; (2) a settled manifest on
	// disk returns a fresh terminal-success task echoing it.
	m.snapshotInFlightMu.Lock()
	if existing, ok := m.snapshotInFlight[key]; ok {
		m.snapshotInFlightMu.Unlock()
		return existing, nil
	}
	man, present, err := readSnapshotManifest(snapshotManifestPath(poolRoot, spec.Name))
	if err != nil {
		m.snapshotInFlightMu.Unlock()
		return nil, err
	}
	if present {
		m.snapshotInFlightMu.Unlock()
		return m.terminalSnapshotTask(man), nil
	}
	task := m.tasks.Create(TaskKindVMSnapshotCreate, v.ID)
	m.snapshotInFlight[key] = task
	m.snapshotInFlightMu.Unlock()

	// #nosec G118 -- async task work intentionally outlives the HTTP request;
	// clients track progress through GET /v1/tasks/{id}.
	go m.runSnapshotCreate(task.ID, key, v, poolRoot, spec)
	return task, nil
}

// terminalSnapshotTask mints a fresh terminal-success task whose result echoes
// an already-materialized manifest, for a redelivered create that must NOT
// re-capture. The manifest itself is never re-derived.
func (m *Manager) terminalSnapshotTask(man snapshotManifest) *AgentTask {
	t := m.tasks.Create(TaskKindVMSnapshotCreate, man.VMUUID)
	result, _ := json.Marshal(man.toAPI())
	m.tasks.Update(t.ID, func(at *AgentTask) {
		at.Status = TaskStatusSuccess
		at.Result = result
	})
	return m.tasks.Get(t.ID)
}

// runSnapshotCreate captures each VM disk device into a content-addressed
// blob, writes the manifest, and finalizes the task with the agentapi.Snapshot
// the CP worker consumes. It clears the in-flight entry on the way out
// regardless of outcome so a later create can re-attempt (a failed capture
// leaves no manifest, so the next create starts fresh).
func (m *Manager) runSnapshotCreate(taskID uuid.UUID, key snapshotInFlightKey, v *VM, poolRoot string, spec SnapshotSpec) {
	defer func() {
		m.snapshotInFlightMu.Lock()
		delete(m.snapshotInFlight, key)
		m.snapshotInFlightMu.Unlock()
	}()

	ctx := context.Background()
	log := m.log.With("vm_id", v.ID.String(), "task_id", taskID.String(), "snapshot", spec.Name)
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	snapshotsDir := filepath.Join(poolRoot, snapshotsSubdir)
	stagingDir := filepath.Join(snapshotsDir, snapshotsStagingSubdir)
	if err := os.MkdirAll(stagingDir, 0o750); err != nil {
		log.Error("create snapshots staging dir", "err", err)
		m.failSnapshotTask(taskID, "internal", err.Error())
		return
	}

	disks := make([]snapshotManifestDisk, 0)
	for _, dev := range m.snapshotDiskDevices(v) {
		// Pre-create the backup target file the capturer's blockdev-add opens,
		// then capture device into it; produceBlob converts that backup into the
		// content-addressed standalone blob (hash/rename/sidecar).
		backup := filepath.Join(stagingDir, fmt.Sprintf("backup-%s-%s.qcow2", spec.Name, dev.device))
		if err := os.WriteFile(backup, qcow2Magic[:], 0o600); err != nil {
			log.Error("create backup target", "device", dev.device, "err", err)
			m.failSnapshotTask(taskID, "internal", err.Error())
			return
		}
		if err := m.diskCapturer.Capture(ctx, v, dev.device, backup); err != nil {
			_ = os.Remove(backup)
			log.Error("capture disk", "device", dev.device, "err", err)
			m.failSnapshotTask(taskID, "snapshot_capture_failed", err.Error())
			return
		}
		blob, err := produceBlob(ctx, backup, snapshotsDir, m.snapshotConvert)
		_ = os.Remove(backup)
		if err != nil {
			log.Error("produce blob", "device", dev.device, "err", err)
			m.failSnapshotTask(taskID, "snapshot_blob_failed", err.Error())
			return
		}
		disks = append(disks, snapshotManifestDisk{
			Index:     dev.index,
			Device:    dev.device,
			SHA256:    blob.SHA256,
			SizeBytes: blob.SizeBytes,
			Format:    "qcow2",
		})
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].Index < disks[j].Index })

	man := snapshotManifest{
		Name:              spec.Name,
		Description:       spec.Description,
		VMUUID:            v.ID,
		VMName:            v.Name,
		Architecture:      string(v.Architecture),
		VMStateAtSnapshot: snapshotVMState(v.Status),
		CreatedAt:         time.Now().UTC(),
		Disks:             disks,
	}
	if err := writeSnapshotManifest(poolRoot, man); err != nil {
		log.Error("write manifest", "err", err)
		m.failSnapshotTask(taskID, "internal", err.Error())
		return
	}

	result, err := json.Marshal(man.toAPI())
	if err != nil {
		log.Error("encode snapshot result", "err", err)
		m.failSnapshotTask(taskID, "internal", err.Error())
		return
	}
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusSuccess
		t.Result = result
	})
	log.Info("snapshot captured", "disks", len(disks))
}

// snapshotVMState maps the agent VM status to the wire vm_state_at_snapshot.
// Slice A is disk-only and crash-consistent; a running guest's snapshot is
// still disk-only, so the field reports the guest's observed run-state at
// capture time (the CP records it for recreate semantics).
func snapshotVMState(s Status) string {
	switch s {
	case StatusRunning, StatusMigratingIncoming:
		return "running"
	case StatusPaused:
		return "paused"
	default:
		return "stopped"
	}
}

// failSnapshotTask marks the snapshot task failed without touching VM status
// (a snapshot capture failure does not change the VM's lifecycle phase).
func (m *Manager) failSnapshotTask(taskID uuid.UUID, code, message string) {
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusFailed
		t.Error = &TaskError{Code: code, Message: message}
	})
}

// DeleteSnapshot removes the named snapshot's manifest and then GCs the blobs
// it referenced that NO OTHER local manifest references. Fail-closed: a blob
// still referenced by another manifest is never removed; on any uncertainty
// the blob is leaked, never deleted (the CP refgraph is authoritative
// cluster-side - this is best-effort agent-local cleanup of orphans the agent
// owns). Returns the agent task tracking the async work.
func (m *Manager) DeleteSnapshot(ctx context.Context, vmName, snapshotName string) (*AgentTask, error) {
	v, err := m.ByName(vmName)
	if err != nil {
		return nil, err
	}
	poolRoot, err := m.poolRoot(v.PoolName)
	if err != nil {
		return nil, err
	}
	task := m.tasks.Create(TaskKindVMSnapshotDelete, v.ID)
	// #nosec G118 -- async task work intentionally outlives the HTTP request.
	go m.runSnapshotDelete(task.ID, poolRoot, snapshotName)
	return task, nil
}

func (m *Manager) runSnapshotDelete(taskID uuid.UUID, poolRoot, snapshotName string) {
	log := m.log.With("task_id", taskID.String(), "snapshot", snapshotName)
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	manPath := snapshotManifestPath(poolRoot, snapshotName)
	target, present, err := readSnapshotManifest(manPath)
	if err != nil {
		log.Error("read manifest for delete", "err", err)
		m.failSnapshotTask(taskID, "internal", err.Error())
		return
	}
	if !present {
		// Already gone: delete is idempotent, report success.
		m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusSuccess })
		return
	}

	// Remove the manifest FIRST so the surviving-reference scan below cannot
	// count the snapshot being deleted as a live reference to its own blobs.
	if err := os.Remove(manPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Error("remove manifest", "err", err)
		m.failSnapshotTask(taskID, "internal", err.Error())
		return
	}

	// Build the set of digests still referenced by ANY remaining manifest.
	// Fail-closed: if the surviving manifests cannot be enumerated, leak every
	// blob (delete nothing) rather than risk removing one still in use.
	survivors, err := listManifests(poolRoot)
	if err != nil {
		log.Warn("list surviving manifests; leaking blobs (fail-closed)", "err", err)
		m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusSuccess })
		return
	}
	referenced := map[string]bool{}
	for _, man := range survivors {
		for _, d := range man.Disks {
			referenced[d.SHA256] = true
		}
	}

	snapshotsDir := filepath.Join(poolRoot, snapshotsSubdir)
	for _, d := range target.Disks {
		if referenced[d.SHA256] {
			continue // still in use by another snapshot - never delete.
		}
		blobPath := filepath.Join(snapshotsDir, d.SHA256+".qcow2")
		if err := os.Remove(blobPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Warn("remove orphan blob", "sha256", d.SHA256, "err", err)
		}
		if err := os.Remove(blobPath + ".sha256"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Warn("remove orphan blob sidecar", "sha256", d.SHA256, "err", err)
		}
	}

	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusSuccess })
	log.Info("snapshot deleted")
}

// VMSnapshots lists the materialized snapshots for the named VM, projected
// onto the agentapi.Snapshot wire shape and ordered by created-at. Used by the
// list handler and (Task 11) the heartbeat inventory.
func (m *Manager) VMSnapshots(vmName string) ([]agentapi.Snapshot, error) {
	v, err := m.ByName(vmName)
	if err != nil {
		return nil, err
	}
	poolRoot, err := m.poolRoot(v.PoolName)
	if err != nil {
		return nil, err
	}
	mans, err := listManifests(poolRoot)
	if err != nil {
		return nil, err
	}
	out := make([]agentapi.Snapshot, 0, len(mans))
	for _, man := range mans {
		if man.VMUUID != v.ID {
			continue
		}
		out = append(out, man.toAPI())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// VMSnapshot returns the named snapshot for the named VM, or (zero, false)
// when no manifest matches. Used by the get handler.
func (m *Manager) VMSnapshot(vmName, snapshotName string) (agentapi.Snapshot, bool, error) {
	v, err := m.ByName(vmName)
	if err != nil {
		return agentapi.Snapshot{}, false, err
	}
	poolRoot, err := m.poolRoot(v.PoolName)
	if err != nil {
		return agentapi.Snapshot{}, false, err
	}
	man, present, err := readSnapshotManifest(snapshotManifestPath(poolRoot, snapshotName))
	if err != nil {
		return agentapi.Snapshot{}, false, err
	}
	if !present || man.VMUUID != v.ID {
		return agentapi.Snapshot{}, false, nil
	}
	return man.toAPI(), true, nil
}

// poolRoot resolves a pool name to its on-disk root via the registry, or
// ErrPoolUnknown when the pool is not registered.
func (m *Manager) poolRoot(name string) (string, error) {
	m.poolsMu.RLock()
	p, ok := m.pools[name]
	m.poolsMu.RUnlock()
	if !ok {
		return "", ErrPoolUnknown
	}
	return p.root, nil
}
