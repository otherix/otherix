// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/digitalocean/go-qemu/qmp"
)

// BackupTargetNodePrefix is the node-name prefix every transient
// blockdev-backup target node carries (see vm.qemuBackupIDs, which builds
// "snaptgt-<8hex>"). It is the single source of truth for both the producer of
// those nodes and the stale-node cleanup that drops leftovers, so the two can
// never diverge: a node-name with this prefix is always a snapshot backup target
// and never a real VM disk (those are virtio<i>).
const BackupTargetNodePrefix = "snaptgt-"

// backupCompletionTimeout bounds the wait for a single disk backup's
// BLOCK_JOB_COMPLETED. It is generous because a full-copy backup transfers the
// whole disk, and under TCG (no KVM, e.g. the Lima dev stack) the copy is slow;
// a stuck job must still fail the snapshot task rather than wedge it forever.
// Mirrors the live-migration disk-copy bound (liveDefaultDiskMirrorTimeout).
const backupCompletionTimeout = 30 * time.Minute

// NamedBlockNode is one entry of the query-named-block-nodes reply: a named
// node in the guest's block graph. Only node-name is decoded; the
// stale-backup-target cleanup keys solely on the name's prefix.
type NamedBlockNode struct {
	NodeName string `json:"node-name"`
}

// queryNamedBlockNodesCmd builds query-named-block-nodes (no arguments).
func queryNamedBlockNodesCmd() []byte {
	return mustCmd("query-named-block-nodes", nil)
}

// QueryNamedBlockNodes issues query-named-block-nodes and returns the guest's
// named block-graph nodes. The reply is the standard {"return": [ {...} ]}
// envelope.
func (c *QMPClient) QueryNamedBlockNodes() ([]NamedBlockNode, error) {
	raw, err := c.monitor.Run(queryNamedBlockNodesCmd())
	if err != nil {
		return nil, fmt.Errorf("query-named-block-nodes: %w", err)
	}
	var resp struct {
		Return []NamedBlockNode `json:"return"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("query-named-block-nodes decode: %w", err)
	}
	return resp.Return, nil
}

// staleTargetConn is the minimal QMP surface dropStaleBackupTargets drives:
// enumerate the guest's named block nodes and drop one by name. *QMPClient
// satisfies it; tests inject a fake.
type staleTargetConn interface {
	QueryNamedBlockNodes() ([]NamedBlockNode, error)
	BlockdevDel(nodeName string) error
}

var _ staleTargetConn = (*QMPClient)(nil)

// DropStaleBackupTargets best-effort drops every leftover snapshot backup-target
// node (BackupTargetNodePrefix) from the running guest's block graph. A prior
// snapshot task that died between blockdev-add and blockdev-del leaves the
// target node and its open fd in the still-running guest qemu (the agent's death
// does not kill the guest); on a crash-loop these accumulate. This reaps them on
// the next capture entry. It is strictly best-effort: an enumerate failure
// returns nil (the capture must not fail), and qemu's refusal to drop a node
// still in use by an active job is logged and skipped (fail toward inaction). A
// real VM disk (virtio<i>) never carries the prefix and is never touched.
func (c *QMPClient) DropStaleBackupTargets(_ context.Context) error {
	return dropStaleBackupTargets(c, c.staleLogger())
}

// staleLogger returns the logger DropStaleBackupTargets uses. The QMPClient
// carries no logger of its own, so cleanup diagnostics go to the slog default;
// the caller treats the whole operation as best-effort regardless.
func (c *QMPClient) staleLogger() *slog.Logger {
	return slog.Default()
}

// dropStaleBackupTargets enumerates conn's named block nodes and drops every one
// whose name carries BackupTargetNodePrefix. An enumerate error returns nil (do
// not fail the capture). A per-node blockdev-del error (e.g. qemu refusing a node
// still in use by an active job) is logged and skipped; the loop continues. A
// non-prefixed node is never dropped.
func dropStaleBackupTargets(conn staleTargetConn, log *slog.Logger) error {
	nodes, err := conn.QueryNamedBlockNodes()
	if err != nil {
		// Best-effort: an enumeration failure must not fail the capture that
		// follows. With no listing there is nothing to safely drop.
		log.Warn("snapshot cleanup: enumerate block nodes failed; skipping stale-target reap", "err", err)
		return nil
	}
	for _, n := range nodes {
		if !strings.HasPrefix(n.NodeName, BackupTargetNodePrefix) {
			continue // a real VM disk (virtio<i>) or any other node: never touch.
		}
		if delErr := conn.BlockdevDel(n.NodeName); delErr != nil {
			// qemu refuses blockdev-del on a node still in use by an active job.
			// Fail toward inaction: leave it, log, and move on - never propagate.
			log.Warn("snapshot cleanup: drop stale backup target failed; leaving it", "node_name", n.NodeName, "err", delErr)
		}
	}
	return nil
}

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
