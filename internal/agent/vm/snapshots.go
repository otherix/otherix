// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/artifactstore"
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

// snapshotCaptureTimeout is the outer fail-closed bound on a whole snapshot
// capture (all disks plus the qemu-img create/convert steps). A capture that
// runs past it makes the task FAIL instead of wedging in "creating" forever.
// It is generous - a full-copy backup transfers the whole disk and is slow
// under TCG - and sits above the per-disk backupCompletionTimeout so a single
// stuck disk surfaces through BackupDiskToFile's own bound first.
const snapshotCaptureTimeout = 45 * time.Minute

// SnapshotSpec is the post-validation create request the manager acts on.
// Snapshots are disk-only: WithMemory is rejected (RAM capture is out of scope).
type SnapshotSpec struct {
	Name        string
	Description string
	WithMemory  bool
}

// ErrSnapshotWithMemory is returned by CreateSnapshot when with_memory is set.
// Snapshots capture disks only; the CP already rejects this, so reaching the
// agent is defense in depth. Handlers map it to 400 validation_failed.
var ErrSnapshotWithMemory = errors.New("snapshot with_memory is unsupported (disk-only)")

// snapshotNamePattern mirrors internal/api/validation.ValidateSnapshotName. The
// snapshot name is concatenated into agent filesystem paths
// ({vm_uuid}__{name}.json + the per-blob files), so the agent re-validates it
// here as defense in depth: it must not trust the CP to have rejected a
// path-traversal name. The class excludes '/' and '\', making `../` segment
// traversal unrepresentable.
var snapshotNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)

// validSnapshotName reports whether name is safe to embed in an agent path.
func validSnapshotName(name string) bool { return snapshotNamePattern.MatchString(name) }

// maxOrphanedDigests bounds the per-request orphaned-blob list the CP may hand
// the agent to GC. A real snapshot references one blob per disk; a list this far
// above any plausible disk count is anomalous (resource amplification or a bug)
// and is rejected rather than walked.
const maxOrphanedDigests = 4096

// diskCapturer captures one running/stopped VM disk device into a destination
// backup file. The production impl dials the VM's QMP socket and runs
// blockdev-backup; tests inject a fake that copies a fixture qcow2. It is the
// seam that keeps CreateSnapshot unit-testable on darwin without real qemu.
// src is the on-disk source path (used to size the backup target); device is
// the qemu block device name (virtio<i>) that blockdev-backup reads.
type diskCapturer interface {
	Capture(ctx context.Context, v *VM, device, src, dest string) error
}

// qmpDiskCapturer is the production diskCapturer: it creates the backup target
// qcow2, dials the VM's QMP socket, and runs a full-copy blockdev-backup of
// device into that target. Crash-consistent and disk-only.
type qmpDiskCapturer struct{}

// Capture creates the backup target as a real qcow2 sized to the source, then
// backs up device into it via QMP. blockdev-add OPENS the target (it does not
// create it) and qemu rejects a zero/non-qcow2 file ("Unsupported qcow2 version
// 0"), so the target must be a valid qcow2 of the source's virtual size first.
// The source size is read with -U force-share since the running VM holds the
// disk write lock.
func (qmpDiskCapturer) Capture(ctx context.Context, v *VM, device, src, dest string) error {
	size, err := qemu.ImgVirtualSizeShared(ctx, src)
	if err != nil {
		return fmt.Errorf("source disk size %s: %v", src, err)
	}
	if err := qemu.CreateDisk(ctx, dest, size); err != nil {
		return fmt.Errorf("create backup target: %v", err)
	}
	client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial qmp: %v", err)
	}
	defer func() { _ = client.Close() }()
	// Best-effort reap of any leftover snapshot backup-target nodes a prior
	// crashed capture left in this still-running guest (the agent dying between
	// blockdev-add and blockdev-del does not kill the guest, so the node + its
	// open fd linger). DropStaleBackupTargets is fail-toward-inaction; its error
	// is discarded so a cleanup failure NEVER fails the capture that follows.
	// Within this one Capture the backup adds and drops its own node before
	// returning, so this entry-time reap only ever sees truly-stale nodes from
	// PRIOR tasks.
	_ = client.DropStaleBackupTargets(ctx)
	jobID, nodeName := qemuBackupIDs()
	if err := client.BackupDiskToFile(ctx, jobID, nodeName, device, dest); err != nil {
		return fmt.Errorf("backup disk %s: %v", device, err)
	}
	return nil
}

// qemuBackupIDs returns a (jobID, nodeName) pair for one transient
// blockdev-backup. Both stay within QEMU's 31-char node-name limit: an 8-hex
// suffix is unique enough for identifiers that live only for the duration of a
// single backup in one VM's block graph. A full uuid (36 chars) overflows the
// limit and qemu rejects blockdev-add with "Node name too long".
func qemuBackupIDs() (jobID, nodeName string) {
	s := uuid.NewString()[:8]
	return "snap-" + s, qemu.BackupTargetNodePrefix + s
}

// snapshotDiskDevice is one VM disk to capture: its virtio index, the wire
// device name (virtio<i>), and the on-disk source path. Capture currently covers
// the boot disk only (index 0); the index-ordered shape is kept so a
// future multi-disk VM maps blobs to disks deterministically (the same
// virtio<i> invariant the migration data-path relies on).
type snapshotDiskDevice struct {
	index  int
	device string
	src    string
}

// snapshotDiskDevices enumerates v's disks in virtio-index order. Current VMs
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

	// fsync the staged blob content before the rename so the bytes are durable
	// before the rename names them. The staging file was written by an external
	// convert process, so open it explicitly to flush its content: on a power
	// loss the rename must never become visible ahead of the data blocks (which
	// would leave a valid-named blob with garbage content that ListSnapshots
	// advertises as a healthy holder).
	if err := syncFile(tempPath); err != nil {
		return BlobResult{}, fmt.Errorf("sync staging blob: %v", err)
	}
	if err := os.Rename(tempPath, blobPath); err != nil {
		return BlobResult{}, fmt.Errorf("atomic rename to snapshots: %v", err)
	}
	// fsync the snapshots dir so the rename itself is durable. On a sync failure
	// the blob is on disk but not durably linked; surface the error so the caller
	// retries rather than falsely reporting success.
	if err := syncDir(snapshotsDir); err != nil {
		return BlobResult{}, err
	}
	if err := os.WriteFile(sidecarPath, []byte(sha), 0o644); err != nil { //nolint:gosec // sidecar is non-secret metadata
		return BlobResult{}, fmt.Errorf("write snapshot sidecar: %v", err)
	}
	// fsync the snapshots dir again so the sidecar entry is durable.
	if err := syncDir(snapshotsDir); err != nil {
		return BlobResult{}, err
	}
	return BlobResult{SHA256: sha, Path: blobPath, SizeBytes: size}, nil
}

// syncFile opens path read-only, fsyncs it, and closes it, flushing content
// written by an external process so the bytes are durable before a subsequent
// rename names the file. A sync failure is reported, not swallowed.
func syncFile(path string) error {
	f, err := os.Open(path) // #nosec G304 -- staging path is built from the agent's own pool root, fresh uuid
	if err != nil {
		return fmt.Errorf("open file for sync: %v", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync file: %v", err)
	}
	return f.Close()
}

// syncDir fsyncs a directory so a rename or create within it is durable across a
// power loss. A sync failure on the parent dir is reported, not swallowed, since
// it defeats the crash-durability guarantee.
func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- store-internal directory path
	if err != nil {
		return fmt.Errorf("open dir for sync: %v", err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("sync dir: %v", err)
	}
	return d.Close()
}

// produceBlobToStore materializes srcDisk (a backup-target qcow2) into the
// dedicated content-addressed artifact store, mirroring produceBlob's
// convert + validate + hash tail but landing the blob in the per-node store
// instead of the disk pool's snapshots/ dir:
//
//  1. convert srcDisk into a fresh standalone qcow2 in a private temp dir;
//  2. validate its qcow2 magic and hash it;
//  3. hand the staged file to store.Put, which re-hashes the stream, rejects a
//     digest mismatch, and atomically renames it into blobs/{sha} (idempotent: an
//     identical blob already present is a no-op).
//
// The blob is never visible under blobs/{sha} until it validates (store.Put's
// contract), and the temp dir is removed on the way out, so a crash leaves no
// partial blob. This is the additive transport copy; the disk-pool produceBlob
// copy stays authoritative for idempotency/list/delete.
func produceBlobToStore(ctx context.Context, srcDisk string, store *artifactstore.Store, convert convertFunc) (BlobResult, error) {
	tmpDir, err := os.MkdirTemp("", "otherix-blob-")
	if err != nil {
		return BlobResult{}, fmt.Errorf("create blob temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	staged := filepath.Join(tmpDir, uuid.NewString()+".qcow2")

	if err := convert(ctx, srcDisk, staged); err != nil {
		return BlobResult{}, fmt.Errorf("convert %s into staging blob: %v", srcDisk, err)
	}
	if err := validateQcow2Magic(staged); err != nil {
		return BlobResult{}, fmt.Errorf("qcow2_header_invalid: %v", err)
	}
	sha, err := hashFile(staged)
	if err != nil {
		return BlobResult{}, fmt.Errorf("hash staging blob: %v", err)
	}
	info, err := os.Stat(staged)
	if err != nil {
		return BlobResult{}, fmt.Errorf("stat staging blob: %v", err)
	}
	f, err := os.Open(staged) //nolint:gosec // staged is a fresh temp file produced above
	if err != nil {
		return BlobResult{}, fmt.Errorf("open staging blob: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := store.Put(sha, f); err != nil {
		return BlobResult{}, fmt.Errorf("put blob into artifact store: %v", err)
	}
	return BlobResult{SHA256: sha, Path: store.Root(), SizeBytes: info.Size()}, nil
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

// ListSnapshots resolves poolName to its registered root and returns the
// inventory of content-addressed snapshot blobs under {pool.root}/snapshots/.
// Mirrors Manager.ListImages: returns ErrPoolUnknown for unknown pools, and
// a nil slice with nil error when the snapshots dir is absent.
func (m *Manager) ListSnapshots(_ context.Context, poolName string) ([]SnapshotBlob, error) {
	m.poolsMu.RLock()
	p, ok := m.pools[poolName]
	m.poolsMu.RUnlock()
	if !ok {
		return nil, ErrPoolUnknown
	}
	return ListSnapshots(filepath.Join(p.root, snapshotsSubdir))
}

// snapshotManifestPath returns the on-disk manifest path, scoped by VM so two
// different VMs (or a reused VM name) cannot collide on the same snapshot name:
// {root}/snapshots/manifests/{vm_uuid}__{name}.json. Snapshot names are unique
// only per-VM (the CP enforces that), so a pool-global name-only path would let
// one VM's snapshot overwrite or be returned for another's.
func snapshotManifestPath(poolRoot string, vmUUID uuid.UUID, name string) string {
	return filepath.Join(poolRoot, snapshotsSubdir, snapshotManifestsSubdir, vmUUID.String()+"__"+name+".json")
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
	if err := os.Rename(tmp, snapshotManifestPath(poolRoot, man.VMUUID, man.Name)); err != nil {
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
// and returns the agent task tracking it. WithMemory is rejected (snapshots are
// disk-only). Idempotency is (vm, snapshot_name)-keyed defense in depth: a
// redelivered create whose manifest already exists OR whose capture is still
// in flight returns the existing task/manifest and NEVER starts a second
// blockdev-backup - closing the CP-side worker-redelivery residual window.
func (m *Manager) CreateSnapshot(ctx context.Context, vmName string, spec SnapshotSpec) (*AgentTask, error) {
	if spec.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidSpec)
	}
	if !validSnapshotName(spec.Name) {
		return nil, fmt.Errorf("%w: invalid snapshot name", ErrInvalidSpec)
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

	key := snapshotInFlightKey{vmUUID: v.ID, snapshotName: spec.Name}

	// Single critical section over both idempotency gates so two concurrent
	// first attempts cannot both launch a capture: (1) an in-flight capture
	// for the same key returns the original task; (2) a settled manifest on
	// disk returns a fresh terminal-success task echoing it.
	m.snapshotInFlightMu.Lock()
	if existing, ok := m.snapshotInFlight[key]; ok {
		m.snapshotInFlightMu.Unlock()
		return existing, nil
	}
	man, present, err := readSnapshotManifest(snapshotManifestPath(poolRoot, v.ID, spec.Name))
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

	// Bound the whole capture so a stuck/slow-beyond-limit backup fails the
	// snapshot task (snapshot_capture_failed) rather than wedging it in
	// "creating" forever. BackupDiskToFile is itself internally bounded
	// (backupCompletionTimeout per disk); this outer deadline is the
	// belt-and-suspenders cap across all disks and the non-QMP steps (qemu-img
	// create/convert) and propagates cancellation into them. It is generous:
	// each disk's full-copy backup is slow under TCG.
	ctx, cancel := context.WithTimeout(context.Background(), snapshotCaptureTimeout)
	defer cancel()
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
		// Capture device into a backup file (the capturer creates the target
		// qcow2 sized to the source), then produceBlob converts that backup into
		// the content-addressed standalone blob (hash/rename/sidecar).
		backup := filepath.Join(stagingDir, fmt.Sprintf("backup-%s-%s.qcow2", spec.Name, dev.device))
		if err := m.diskCapturer.Capture(ctx, v, dev.device, dev.src, backup); err != nil {
			_ = os.Remove(backup)
			log.Error("capture disk", "device", dev.device, "err", err)
			m.failSnapshotTask(taskID, "snapshot_capture_failed", err.Error())
			return
		}
		blob, err := produceBlob(ctx, backup, snapshotsDir, m.snapshotConvert)
		if err != nil {
			_ = os.Remove(backup)
			log.Error("produce blob", "device", dev.device, "err", err)
			m.failSnapshotTask(taskID, "snapshot_blob_failed", err.Error())
			return
		}
		// Additive dual-write: ALSO land the blob in the dedicated per-node
		// artifact store. The disk-pool produceBlob copy above stays AUTHORITATIVE
		// for idempotency/list/delete; this store copy is the additive
		// transport copy. Nil-guarded so a half-wired test path cannot panic;
		// production always has the store set (New fails closed otherwise).
		if m.artifactStore != nil {
			if _, err := produceBlobToStore(ctx, backup, m.artifactStore, m.snapshotConvert); err != nil {
				_ = os.Remove(backup)
				log.Error("produce blob into artifact store", "device", dev.device, "err", err)
				m.failSnapshotTask(taskID, "snapshot_blob_failed", err.Error())
				return
			}
		}
		_ = os.Remove(backup)
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

	// Additive manifest dual-write: ALSO write the snapshot manifest into the
	// artifact store, keyed by EACH disk's blob digest, so a peer that pulls any
	// one blob can recover the snapshot's disk set without the disk-pool copy. The
	// disk-pool writeSnapshotManifest above stays AUTHORITATIVE for
	// list/idempotency/delete; this is the additive transport copy. Same manifest
	// JSON the disk-pool copy carries (json.MarshalIndent of snapshotManifest).
	// Nil-guarded; fail-open per disk - a store-side manifest write failure must not
	// fail an already-durable capture (the disk-pool copy is the source of
	// truth), so it logs and continues.
	if m.artifactStore != nil {
		if manJSON, err := json.MarshalIndent(man, "", "  "); err != nil {
			log.Warn("encode artifact-store manifest", "err", err)
		} else {
			for _, d := range disks {
				if err := m.artifactStore.WriteManifest(d.SHA256, manJSON); err != nil {
					log.Warn("write artifact-store manifest", "digest", d.SHA256, "err", err)
				}
			}
		}
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
// Snapshots are disk-only and crash-consistent; a running guest's snapshot is
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

// DeleteSnapshotByID removes the snapshot named snapshotName captured from the
// VM with id vmUUID: its manifest (located by vm_uuid across the agent's
// registered pools, NOT via the live VM, so the source VM may already be gone)
// and the content-addressed blobs the CP marked orphaned that no surviving
// local manifest still references. Fail-closed: a blob referenced by another
// manifest, a blob not in the CP-authoritative orphaned set, or a pool whose
// manifest layer errors is never touched (leak over wrongful delete). The CP
// refgraph is authoritative cluster-side; this is the agent-local execution of
// the orphans the CP already proved unreferenced. Returns the agent task
// tracking the async work.
func (m *Manager) DeleteSnapshotByID(_ context.Context, vmUUID uuid.UUID, snapshotName string, orphaned []string) (*AgentTask, error) {
	// Re-validate at the agent edge (defense in depth): snapshotName and every
	// digest are concatenated into filesystem paths, so a path-traversal name or a
	// non-hex digest must never reach os.Remove. The agent does not trust the CP to
	// have rejected them.
	if !validSnapshotName(snapshotName) {
		return nil, fmt.Errorf("%w: invalid snapshot name", ErrInvalidSpec)
	}
	if len(orphaned) > maxOrphanedDigests {
		return nil, fmt.Errorf("%w: too many orphaned digests (%d)", ErrInvalidSpec, len(orphaned))
	}
	// Drop any non-content-address digest rather than acting on it (a malformed or
	// traversal-shaped digest is a bug/attack signal; fail toward inaction).
	safe := make([]string, 0, len(orphaned))
	for _, d := range orphaned {
		if isHexSHA256Lower(d) {
			safe = append(safe, d)
		} else {
			m.log.Warn("snapshot delete: dropping non-hex orphaned digest", "digest", d)
		}
	}
	task := m.tasks.Create(TaskKindVMSnapshotDelete, vmUUID)
	// #nosec G118 -- async task work intentionally outlives the HTTP request.
	go m.runSnapshotDelete(task.ID, vmUUID, snapshotName, safe)
	return task, nil
}

func (m *Manager) runSnapshotDelete(taskID, vmUUID uuid.UUID, snapshotName string, orphaned []string) {
	log := m.log.With("task_id", taskID.String(), "vm_id", vmUUID.String(), "snapshot", snapshotName)
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	orphanSet := make(map[string]bool, len(orphaned))
	for _, d := range orphaned {
		orphanSet[d] = true
	}

	// Snapshot manifests/blobs are content-addressed under each pool's snapshots/
	// dir; the source VM (hence its pool) may be gone, so sweep every registered
	// pool and act wherever the manifest/blobs physically live.
	m.poolsMu.RLock()
	roots := make([]string, 0, len(m.pools))
	for _, p := range m.pools {
		roots = append(roots, p.root)
	}
	m.poolsMu.RUnlock()

	for _, root := range roots {
		m.gcSnapshotInPool(log, root, vmUUID, snapshotName, orphanSet)
	}

	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusSuccess })
	log.Info("snapshot deleted")
}

// gcSnapshotInPool removes the snapshot's manifest (if present in this pool) and
// then GCs the orphaned blobs this pool holds that no SURVIVING manifest
// references. Fail-closed: any manifest-layer error leaks the whole pool (touches
// nothing), and a blob either still locally referenced or absent from the
// CP-authoritative orphaned set is never removed.
func (m *Manager) gcSnapshotInPool(log *slog.Logger, poolRoot string, vmUUID uuid.UUID, snapshotName string, orphanSet map[string]bool) {
	manPath := snapshotManifestPath(poolRoot, vmUUID, snapshotName)
	_, present, err := readSnapshotManifest(manPath)
	if err != nil {
		log.Warn("read manifest for delete; leaking pool (fail-closed)", "pool_root", poolRoot, "err", err)
		return
	}
	// Remove the manifest FIRST so the surviving-reference scan below cannot count
	// the snapshot being deleted as a live reference to its own blobs.
	if present {
		if err := os.Remove(manPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Warn("remove manifest; leaking pool (fail-closed)", "pool_root", poolRoot, "err", err)
			return
		}
	}

	if len(orphanSet) == 0 {
		return // manifest-only cleanup (every referenced blob is shared)
	}

	// Build the set of digests still referenced by ANY surviving manifest.
	// Fail-closed: if the surviving manifests cannot be enumerated, leak every
	// blob (delete nothing) rather than risk removing one still in use.
	survivors, err := listManifests(poolRoot)
	if err != nil {
		log.Warn("list surviving manifests; leaking blobs (fail-closed)", "pool_root", poolRoot, "err", err)
		return
	}
	referenced := map[string]bool{}
	for _, man := range survivors {
		for _, d := range man.Disks {
			referenced[d.SHA256] = true
		}
	}

	snapshotsDir := filepath.Join(poolRoot, snapshotsSubdir)
	for digest := range orphanSet {
		if referenced[digest] {
			continue // still referenced by a surviving manifest - never delete.
		}
		removeBlobIfPresent(log, snapshotsDir, digest)
	}
}

// removeBlobIfPresent deletes the content-addressed blob (and its sidecar) for
// digest under snapshotsDir, if present. Absence means the blob lives in another
// pool (or was already removed): a no-op, not an error.
func removeBlobIfPresent(log *slog.Logger, snapshotsDir, digest string) {
	blobPath := filepath.Join(snapshotsDir, digest+".qcow2")
	if _, statErr := os.Stat(blobPath); statErr != nil {
		return // blob not in this pool
	}
	if err := os.Remove(blobPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warn("remove orphan blob", "sha256", digest, "err", err)
	}
	if err := os.Remove(blobPath + ".sha256"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warn("remove orphan blob sidecar", "sha256", digest, "err", err)
	}
}

// relocateSnapshotsToStore is a one-time, best-effort, fail-open sweep that moves
// existing disk-pool-resident snapshot blobs (and manifests) into the dedicated
// artifact store. For each {poolRoot}/snapshots/{sha}.qcow2
// it streams the blob into store.Put (which re-verifies the digest from the
// FILENAME and atomically lands it); the old copy is removed ONLY after the blob
// is confirmed present (Has) in the store. Any error leaks the old copy (left
// readable for the dual-read transition) rather than deleting it - blobs are
// immutable and content-addressed, so a failed move costs a redundant copy or a
// re-pull, never data loss. Manifests are copied likewise (fail-open); the
// disk-pool copies stay for idempotency/list/delete (their removal is a later step).
//
// The digest handed to store.Put is taken from the blob FILENAME, NOT the
// sidecar, so store.Put's re-hash is the verification gate: a corrupt blob whose
// content does not hash to its filename digest is rejected and its old copy is
// left intact (fail-open). The old blob+sidecar are removed only after store.Has
// confirms the blob is durably present in the store.
func relocateSnapshotsToStore(log *slog.Logger, poolRoots []string, store *artifactstore.Store) {
	if store == nil {
		return
	}
	for _, root := range poolRoots {
		snapshotsDir := filepath.Join(root, snapshotsSubdir)
		relocateSnapshotsInPool(log, snapshotsDir, root, store)
	}
}

// relocateSnapshotsInPool relocates every content-addressed blob (and the
// snapshot manifests) under one pool's snapshots dir into the store, fail-open.
func relocateSnapshotsInPool(log *slog.Logger, snapshotsDir, poolRoot string, store *artifactstore.Store) {
	entries, err := os.ReadDir(snapshotsDir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warn("relocate: read snapshots dir; skipping pool (fail-open)", "snapshots_dir", snapshotsDir, "err", err)
		}
		return // absent snapshots dir is the common case (no snapshots in this pool)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".qcow2") {
			continue // sidecars and other files
		}
		// The digest is the FILENAME (minus .qcow2); store.Put re-hashes the
		// content and rejects a mismatch, so a corrupt blob never lands and its
		// old copy is left intact. A non-hex filename is skipped (scratch/garbage).
		digest := strings.TrimSuffix(name, ".qcow2")
		if !isHexSHA256Lower(digest) {
			continue
		}
		relocateBlob(log, snapshotsDir, digest, store)
	}
	relocateManifestsToStore(log, poolRoot, store)
}

// relocateBlob moves one content-addressed blob into the store, fail-open. The
// old blob+sidecar are removed ONLY after store.Has confirms the store copy is
// durably present (digest-verified by store.Put). On ANY error the old copy is
// left readable (leak over delete-before-verify).
func relocateBlob(log *slog.Logger, snapshotsDir, digest string, store *artifactstore.Store) {
	blobPath := filepath.Join(snapshotsDir, digest+".qcow2")

	// Already present (verified by a prior sweep or a dual-write capture): the
	// store copy is digest-confirmed, so drop the old copy.
	if store.Has(digest) {
		removeRelocatedBlob(log, snapshotsDir, digest)
		return
	}

	f, err := os.Open(blobPath) //nolint:gosec // agent-owned pool path; digest is validated hex
	if err != nil {
		log.Warn("relocate: open blob; leaking (fail-open)", "sha256", digest, "err", err)
		return
	}
	putErr := store.Put(digest, f)
	_ = f.Close()
	if putErr != nil {
		// Digest mismatch (corrupt blob) or any I/O error: never remove the old
		// copy. store.Put discards the staging file on mismatch, so nothing
		// partial is left in the store.
		log.Warn("relocate: put blob; leaking (fail-open)", "sha256", digest, "err", putErr)
		return
	}
	if !store.Has(digest) {
		// Put reported success but the blob is not visible: do not trust it,
		// leak the old copy.
		log.Warn("relocate: blob absent after put; leaking (fail-open)", "sha256", digest)
		return
	}
	removeRelocatedBlob(log, snapshotsDir, digest)
}

// removeRelocatedBlob deletes the old disk-pool blob + sidecar for digest. Called
// ONLY after store.Has(digest) confirms the store copy is durably present. A
// removal error logs and leaves the file (a harmless leak, reclaimed by C3).
func removeRelocatedBlob(log *slog.Logger, snapshotsDir, digest string) {
	blobPath := filepath.Join(snapshotsDir, digest+".qcow2")
	if err := os.Remove(blobPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warn("relocate: remove old blob", "sha256", digest, "err", err)
	}
	if err := os.Remove(blobPath + ".sha256"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warn("relocate: remove old blob sidecar", "sha256", digest, "err", err)
	}
}

// relocateManifestsToStore copies each disk-pool snapshot manifest into the
// store, keyed by EACH referenced disk digest (mirroring the C1 capture-time
// dual-write), so a peer pulling any one blob can recover the snapshot's disk
// set. Fail-open: a manifest that cannot be read/encoded/written is skipped; the
// disk-pool copy stays authoritative and is left intact (manifest removal is C3).
func relocateManifestsToStore(log *slog.Logger, poolRoot string, store *artifactstore.Store) {
	mans, err := listManifests(poolRoot)
	if err != nil {
		log.Warn("relocate: list manifests; skipping (fail-open)", "pool_root", poolRoot, "err", err)
		return
	}
	for _, man := range mans {
		body, err := json.MarshalIndent(man, "", "  ")
		if err != nil {
			log.Warn("relocate: encode manifest; skipping (fail-open)", "snapshot", man.Name, "err", err)
			continue
		}
		for _, d := range man.Disks {
			if err := store.WriteManifest(d.SHA256, body); err != nil {
				log.Warn("relocate: write manifest; leaking (fail-open)", "digest", d.SHA256, "err", err)
			}
		}
	}
}

// VMSnapshots lists the materialized snapshots for the named VM, projected
// onto the agentapi.Snapshot wire shape and ordered by created-at. Used by the
// list handler and the heartbeat inventory.
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
	man, present, err := readSnapshotManifest(snapshotManifestPath(poolRoot, v.ID, snapshotName))
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
