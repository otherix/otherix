// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"context"
	"fmt"
)

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

// BackupDiskToFile runs one full-copy disk backup end to end: add the qcow2
// file target node, start the blockdev-backup job, await BLOCK_JOB_COMPLETED,
// then drop the target node from the block graph. nodeName and jobID are
// caller-unique per disk. The qcow2 file at filename must already exist. The
// completed file is a standalone, point-in-time copy of device suitable for
// content-addressed hashing.
func (c *QMPClient) BackupDiskToFile(ctx context.Context, jobID, nodeName, device, filename string) error {
	ch, err := c.Events(ctx)
	if err != nil {
		return fmt.Errorf("subscribe events: %w", err)
	}
	if err := c.BlockdevAddQcow2File(nodeName, filename); err != nil {
		return err
	}
	if err := c.BlockdevBackup(jobID, device, nodeName); err != nil {
		// Best-effort cleanup of the orphaned target node.
		_ = c.BlockdevDel(nodeName)
		return err
	}
	if err := waitBlockJobEvent(ctx, ch, "BLOCK_JOB_COMPLETED", jobID); err != nil {
		_ = c.BlockdevDel(nodeName)
		return fmt.Errorf("await backup completion: %w", err)
	}
	if err := c.BlockdevDel(nodeName); err != nil {
		return fmt.Errorf("drop backup target node: %w", err)
	}
	return nil
}
