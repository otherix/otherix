// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"context"
	"fmt"
	"time"

	"github.com/digitalocean/go-qemu/qmp"
)

// backupCompletionTimeout bounds the wait for a single disk backup's
// BLOCK_JOB_COMPLETED. It is generous because a full-copy backup transfers the
// whole disk, and under TCG (no KVM, e.g. the Lima dev stack) the copy is slow;
// a stuck job must still fail the snapshot task rather than wedge it forever.
// Mirrors the live-migration disk-copy bound (liveDefaultDiskMirrorTimeout).
const backupCompletionTimeout = 30 * time.Minute

// blockdevBackupCmd builds blockdev-backup: a full-sync point-in-time copy
// of device into target (a separately-added target node). sync=full so the
// produced blob is a standalone, self-contained image rather than an
// incremental delta - the content-addressed snapshot model requires the
// full disk state in one artifact.
func blockdevBackupCmd(jobID, device, target string) []byte {
	return mustCmd("blockdev-backup", map[string]any{
		"job-id": jobID,
		"device": device,
		"target": target,
		"sync":   "full",
	})
}

// BlockdevBackup starts the full-copy backup job (see blockdevBackupCmd).
// The caller must have added the target node first (BlockdevAddQcow2File)
// and awaits BLOCK_JOB_COMPLETED via the event helpers.
func (c *QMPClient) BlockdevBackup(jobID, device, target string) error {
	if _, err := c.monitor.Run(blockdevBackupCmd(jobID, device, target)); err != nil {
		return fmt.Errorf("blockdev-backup: %w", err)
	}
	return nil
}

// blockdevAddQcow2FileCmd builds blockdev-add for a qcow2-over-file target
// node: a fresh on-disk qcow2 image at filename, wrapped by the file
// protocol driver. This is the backup target the blockdev-backup job writes
// into; it must exist in the block graph before the backup starts.
func blockdevAddQcow2FileCmd(nodeName, filename string) []byte {
	return mustCmd("blockdev-add", map[string]any{
		"driver":    "qcow2",
		"node-name": nodeName,
		"file": map[string]any{
			"driver":   "file",
			"filename": filename,
		},
	})
}

// BlockdevAddQcow2File adds the qcow2-over-file backup target node (see
// blockdevAddQcow2FileCmd). The qcow2 image at filename must already exist
// (created via qemu-img create); blockdev-add opens it, it does not create.
func (c *QMPClient) BlockdevAddQcow2File(nodeName, filename string) error {
	if _, err := c.monitor.Run(blockdevAddQcow2FileCmd(nodeName, filename)); err != nil {
		return fmt.Errorf("blockdev-add qcow2 file: %w", err)
	}
	return nil
}

// BackupConn is the QMP surface runBackup drives for one disk backup. It
// exposes the multiplexed event channel directly so the package-private
// fan-out + block-job waiter (shared with the live-migration path) are reused
// unchanged. *QMPClient satisfies it.
type BackupConn interface {
	Events(ctx context.Context) (<-chan qmp.Event, error)
	BlockdevAddQcow2File(nodeName, filename string) error
	BlockdevBackup(jobID, device, target string) error
	BlockdevDel(nodeName string) error
}

var _ BackupConn = (*QMPClient)(nil)

// BackupDiskToFile runs one full-copy disk backup end to end: add the qcow2
// file target node, start the blockdev-backup job, await BLOCK_JOB_COMPLETED,
// then drop the target node from the block graph. nodeName and jobID are
// caller-unique per disk. The qcow2 file at filename must already exist. The
// completed file is a standalone, point-in-time copy of device suitable for
// content-addressed hashing. The wait is bounded by backupCompletionTimeout so
// a stuck job fails the snapshot rather than hanging forever.
func (c *QMPClient) BackupDiskToFile(ctx context.Context, jobID, nodeName, device, filename string) error {
	return runBackup(ctx, c, jobID, nodeName, device, filename, backupCompletionTimeout)
}

// runBackup drives one disk backup to completion or fails closed. It mirrors
// the live-migration event handling that solved the same QMP-event-loss
// problem: go-qemu demultiplexes command responses AND async events out of ONE
// goroutine over an unbuffered channel, so a blockdev-backup whose
// BLOCK_JOB_COMPLETED reaches the wire before its own command reply wedges the
// listener unless the event channel is continuously drained. fanoutEvents
// buffers events from the moment we subscribe (before issuing the commands), so
// the completion event cannot be lost while the commands run; waitBlockJobsTimeout
// then waits with a fail-closed deadline and surfaces a BLOCK_JOB_ERROR / errored
// BLOCK_JOB_COMPLETED instead of waiting it out. The target node is dropped (best
// effort) on every failure path.
func runBackup(ctx context.Context, conn BackupConn, jobID, nodeName, device, filename string, timeout time.Duration) error {
	rawCh, err := conn.Events(ctx)
	if err != nil {
		return fmt.Errorf("subscribe events: %w", err)
	}
	// Drain rawCh into an in-memory buffer for the lifetime of this backup so
	// the completion event the commands below trigger is never dropped between
	// subscription and the wait. Its lifetime is an independent drainCtx
	// cancelled by the deferred stopDrain.
	drainCtx, stopDrain := context.WithCancel(context.Background())
	defer stopDrain()
	ch := fanoutEvents(drainCtx, rawCh)

	if err := conn.BlockdevAddQcow2File(nodeName, filename); err != nil {
		return err
	}
	if err := conn.BlockdevBackup(jobID, device, nodeName); err != nil {
		// Best-effort cleanup of the orphaned target node.
		_ = conn.BlockdevDel(nodeName)
		return err
	}
	if err := waitBlockJobsTimeout(ctx, ch, "BLOCK_JOB_COMPLETED", []string{jobID}, timeout); err != nil {
		_ = conn.BlockdevDel(nodeName)
		return fmt.Errorf("await backup completion: %w", err)
	}
	if err := conn.BlockdevDel(nodeName); err != nil {
		return fmt.Errorf("drop backup target node: %w", err)
	}
	return nil
}
