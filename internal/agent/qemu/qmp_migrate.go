// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package qemu

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/digitalocean/go-qemu/qmp"
)

// LiveParams carries the migration tuning knobs surfaced through
// migrate-set-parameters. Only the fields that are set (non-empty
// string / non-zero number) are emitted into the QMP arguments map, so
// the source and target setups can each supply just the parameters they
// need.
type LiveParams struct {
	TLSCreds             string
	TLSHostname          string
	TLSAuthz             string
	DowntimeMs           int
	MaxBandwidthBytes    int64
	CPUThrottleInitial   int
	CPUThrottleIncrement int
}

// MigrateInfo is the decoded query-migrate reply: the migration status
// string plus the nested RAM progress counters the watchdog uses to
// reason about convergence.
type MigrateInfo struct {
	Status string `json:"status"`
	RAM    struct {
		Remaining      int64 `json:"remaining"`
		Transferred    int64 `json:"transferred"`
		Total          int64 `json:"total"`
		DirtyPagesRate int64 `json:"dirty-pages-rate"`
		DirtySyncCount int64 `json:"dirty-sync-count"`
	} `json:"ram"`
	// Top-level timing counters, populated by QEMU once the migration has
	// progressed/completed (milliseconds). Surfaced as final migration stats.
	TotalTimeMs int64 `json:"total-time"`
	DowntimeMs  int64 `json:"downtime"`
	SetupTimeMs int64 `json:"setup-time"`
}

// BlockJobInfo is the decoded query-block-jobs entry the live-migration
// reporter uses to surface disk-mirror progress (offset/len) and readiness.
type BlockJobInfo struct {
	Type   string `json:"type"`
	Device string `json:"device"`
	Len    int64  `json:"len"`
	Offset int64  `json:"offset"`
	Busy   bool   `json:"busy"`
	Ready  bool   `json:"ready"`
	Status string `json:"status"`
	Speed  int64  `json:"speed"`
}

// mustCmd marshals a QMP command envelope. args==nil omits the
// "arguments" key entirely (for verbs that take no arguments). A
// marshal failure is a programming error (only string/number/bool/map
// values are ever passed), so it panics rather than threading an error
// through every pure builder.
func mustCmd(execute string, args map[string]any) []byte {
	m := map[string]any{"execute": execute}
	if args != nil {
		m["arguments"] = args
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(fmt.Sprintf("qmp marshal %s: %v", execute, err))
	}
	return b
}

// objectAddTLSCredsCmd builds an object-add for a tls-creds-x509 object
// loading the PEM material from dir. endpoint is "server" or "client";
// verify-peer is always on so both sides authenticate the other.
func objectAddTLSCredsCmd(id, dir, endpoint string) []byte {
	return mustCmd("object-add", map[string]any{
		"qom-type":    "tls-creds-x509",
		"id":          id,
		"dir":         dir,
		"endpoint":    endpoint,
		"verify-peer": true,
	})
}

// ObjectAddTLSCreds adds a tls-creds-x509 object (see
// objectAddTLSCredsCmd).
func (c *QMPClient) ObjectAddTLSCreds(id, dir, endpoint string) error {
	if _, err := c.monitor.Run(objectAddTLSCredsCmd(id, dir, endpoint)); err != nil {
		return fmt.Errorf("object-add tls-creds-x509: %w", err)
	}
	return nil
}

// objectAddAuthzCmd builds an object-add for an authz-simple object that
// restricts the NBD server to the single peer identity (the source
// node's cert distinguished name).
func objectAddAuthzCmd(id, identity string) []byte {
	return mustCmd("object-add", map[string]any{
		"qom-type": "authz-simple",
		"id":       id,
		"identity": identity,
	})
}

// ObjectAddAuthz adds an authz-simple object (see objectAddAuthzCmd).
func (c *QMPClient) ObjectAddAuthz(id, identity string) error {
	if _, err := c.monitor.Run(objectAddAuthzCmd(id, identity)); err != nil {
		return fmt.Errorf("object-add authz-simple: %w", err)
	}
	return nil
}

// nbdServerStartCmd builds nbd-server-start. addr is the legacy tagged
// SocketAddressLegacy form ({type:inet,data:{host,port}}) with port a
// STRING, max-connections is pinned to 1 (one incoming mirror channel).
func nbdServerStartCmd(host string, port int, tlsCreds, tlsAuthz string) []byte {
	return mustCmd("nbd-server-start", map[string]any{
		"addr": map[string]any{
			"type": "inet",
			"data": map[string]any{
				"host": host,
				"port": strconv.Itoa(port),
			},
		},
		"tls-creds":       tlsCreds,
		"tls-authz":       tlsAuthz,
		"max-connections": 1,
	})
}

// NBDServerStart starts the target-side NBD server (see
// nbdServerStartCmd).
func (c *QMPClient) NBDServerStart(host string, port int, tlsCreds, tlsAuthz string) error {
	if _, err := c.monitor.Run(nbdServerStartCmd(host, port, tlsCreds, tlsAuthz)); err != nil {
		return fmt.Errorf("nbd-server-start: %w", err)
	}
	return nil
}

// nbdServerStopCmd builds nbd-server-stop (no arguments).
func nbdServerStopCmd() []byte {
	return mustCmd("nbd-server-stop", nil)
}

// NBDServerStop stops the target-side NBD server (see nbdServerStopCmd).
func (c *QMPClient) NBDServerStop() error {
	if _, err := c.monitor.Run(nbdServerStopCmd()); err != nil {
		return fmt.Errorf("nbd-server-stop: %w", err)
	}
	return nil
}

// blockExportAddCmd builds block-export-add type=nbd, exposing nodeName
// under the opaque export name with the given writability.
func blockExportAddCmd(id, nodeName, name string, writable bool) []byte {
	return mustCmd("block-export-add", map[string]any{
		"type":      "nbd",
		"id":        id,
		"node-name": nodeName,
		"name":      name,
		"writable":  writable,
	})
}

// BlockExportAdd registers an NBD block export (see blockExportAddCmd).
func (c *QMPClient) BlockExportAdd(id, nodeName, name string, writable bool) error {
	if _, err := c.monitor.Run(blockExportAddCmd(id, nodeName, name, writable)); err != nil {
		return fmt.Errorf("block-export-add: %w", err)
	}
	return nil
}

// blockExportDelCmd builds block-export-del for the export id.
func blockExportDelCmd(id string) []byte {
	return mustCmd("block-export-del", map[string]any{"id": id})
}

// BlockExportDel removes an NBD block export (see blockExportDelCmd).
func (c *QMPClient) BlockExportDel(id string) error {
	if _, err := c.monitor.Run(blockExportDelCmd(id)); err != nil {
		return fmt.Errorf("block-export-del: %w", err)
	}
	return nil
}

// ObjectDel removes a QOM object previously added with object-add (the
// migration TLS-creds / authz objects). Used at every terminal outcome so a
// long-lived guest qemu does not carry a stale "migtls" object that would make
// its NEXT migration fail "duplicate property 'migtls'".
func (c *QMPClient) ObjectDel(id string) error {
	if _, err := c.monitor.Run(objectDelCmd(id)); err != nil {
		return fmt.Errorf("object-del: %w", err)
	}
	return nil
}

func objectDelCmd(id string) []byte {
	return mustCmd("object-del", map[string]any{"id": id})
}

// blockdevAddNBDCmd builds blockdev-add driver=nbd. server is the FLAT
// SocketAddress form ({type:inet,host,port}) with port a STRING (no
// nested data); reconnect-delay is pinned to 30s.
func blockdevAddNBDCmd(nodeName, host string, port int, export, tlsCreds, tlsHostname string) []byte {
	return mustCmd("blockdev-add", map[string]any{
		"driver":    "nbd",
		"node-name": nodeName,
		"server": map[string]any{
			"type": "inet",
			"host": host,
			"port": strconv.Itoa(port),
		},
		"export":       export,
		"tls-creds":    tlsCreds,
		"tls-hostname": tlsHostname,
		// discard=unmap + detect-zeroes=unmap so the blockdev-mirror's all-zero
		// writes to this NBD client are sent as compact NBD_CMD_WRITE_ZEROES
		// instead of runs of literal zero bytes over TLS. A sync=full mirror of
		// a sparse disk otherwise ships the source's zero clusters in full,
		// which under slow emulation wedges the transfer mid-copy.
		"discard":         "unmap",
		"detect-zeroes":   "unmap",
		"reconnect-delay": 30,
	})
}

// BlockdevAddNBD adds the source-side NBD client blockdev that the
// mirror writes into (see blockdevAddNBDCmd).
func (c *QMPClient) BlockdevAddNBD(nodeName, host string, port int, export, tlsCreds, tlsHostname string) error {
	if _, err := c.monitor.Run(blockdevAddNBDCmd(nodeName, host, port, export, tlsCreds, tlsHostname)); err != nil {
		return fmt.Errorf("blockdev-add nbd: %w", err)
	}
	return nil
}

// blockdevDelCmd builds blockdev-del for nodeName.
func blockdevDelCmd(nodeName string) []byte {
	return mustCmd("blockdev-del", map[string]any{"node-name": nodeName})
}

// BlockdevDel removes a blockdev node (see blockdevDelCmd).
func (c *QMPClient) BlockdevDel(nodeName string) error {
	if _, err := c.monitor.Run(blockdevDelCmd(nodeName)); err != nil {
		return fmt.Errorf("blockdev-del: %w", err)
	}
	return nil
}

// blockdevMirrorCmd builds blockdev-mirror: a full-sync background mirror
// of device into target, reporting (not ignoring) source/target errors.
func blockdevMirrorCmd(jobID, device, target string) []byte {
	return mustCmd("blockdev-mirror", map[string]any{
		"job-id":          jobID,
		"device":          device,
		"target":          target,
		"sync":            "full",
		"copy-mode":       "background",
		"on-source-error": "report",
		"on-target-error": "report",
	})
}

// BlockdevMirror starts the disk mirror job (see blockdevMirrorCmd).
func (c *QMPClient) BlockdevMirror(jobID, device, target string) error {
	if _, err := c.monitor.Run(blockdevMirrorCmd(jobID, device, target)); err != nil {
		return fmt.Errorf("blockdev-mirror: %w", err)
	}
	return nil
}

// blockJobCancelCmd builds block-job-cancel. force=false is the graceful
// finalize after READY (emits BLOCK_JOB_COMPLETED, dest is a synced copy,
// source NOT pivoted); force=true aborts the job.
func blockJobCancelCmd(device string, force bool) []byte {
	return mustCmd("block-job-cancel", map[string]any{
		"device": device,
		"force":  force,
	})
}

// BlockJobCancel finalizes or aborts a block job (see blockJobCancelCmd).
func (c *QMPClient) BlockJobCancel(device string, force bool) error {
	if _, err := c.monitor.Run(blockJobCancelCmd(device, force)); err != nil {
		return fmt.Errorf("block-job-cancel: %w", err)
	}
	return nil
}

// migrateSetCapabilitiesCmd builds migrate-set-capabilities. The map
// keys are sorted so the emitted capabilities array is deterministic.
func migrateSetCapabilitiesCmd(caps map[string]bool) []byte {
	keys := make([]string, 0, len(caps))
	for k := range caps {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	list := make([]any, 0, len(keys))
	for _, k := range keys {
		list = append(list, map[string]any{
			"capability": k,
			"state":      caps[k],
		})
	}
	return mustCmd("migrate-set-capabilities", map[string]any{
		"capabilities": list,
	})
}

// MigrateSetCapabilities toggles migration capabilities (see
// migrateSetCapabilitiesCmd).
func (c *QMPClient) MigrateSetCapabilities(caps map[string]bool) error {
	if _, err := c.monitor.Run(migrateSetCapabilitiesCmd(caps)); err != nil {
		return fmt.Errorf("migrate-set-capabilities: %w", err)
	}
	return nil
}

// migrateSetParametersCmd builds migrate-set-parameters, emitting only
// the parameters that are set (non-empty string / non-zero number).
func migrateSetParametersCmd(p LiveParams) []byte {
	args := map[string]any{}
	if p.TLSCreds != "" {
		args["tls-creds"] = p.TLSCreds
	}
	if p.TLSHostname != "" {
		args["tls-hostname"] = p.TLSHostname
	}
	if p.TLSAuthz != "" {
		args["tls-authz"] = p.TLSAuthz
	}
	if p.DowntimeMs != 0 {
		args["downtime-limit"] = p.DowntimeMs
	}
	if p.MaxBandwidthBytes != 0 {
		args["max-bandwidth"] = p.MaxBandwidthBytes
	}
	if p.CPUThrottleInitial != 0 {
		args["cpu-throttle-initial"] = p.CPUThrottleInitial
	}
	if p.CPUThrottleIncrement != 0 {
		args["cpu-throttle-increment"] = p.CPUThrottleIncrement
	}
	return mustCmd("migrate-set-parameters", args)
}

// MigrateSetParameters applies migration tuning parameters (see
// migrateSetParametersCmd).
func (c *QMPClient) MigrateSetParameters(p LiveParams) error {
	if _, err := c.monitor.Run(migrateSetParametersCmd(p)); err != nil {
		return fmt.Errorf("migrate-set-parameters: %w", err)
	}
	return nil
}

// migrateCmd builds migrate to the given outgoing uri.
func migrateCmd(uri string) []byte {
	return mustCmd("migrate", map[string]any{"uri": uri})
}

// Migrate starts the outgoing RAM migration (see migrateCmd).
func (c *QMPClient) Migrate(uri string) error {
	if _, err := c.monitor.Run(migrateCmd(uri)); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// migrateIncomingCmd builds migrate-incoming for the listening uri.
func migrateIncomingCmd(uri string) []byte {
	return mustCmd("migrate-incoming", map[string]any{"uri": uri})
}

// MigrateIncoming arms the deferred incoming migration (see
// migrateIncomingCmd).
func (c *QMPClient) MigrateIncoming(uri string) error {
	if _, err := c.monitor.Run(migrateIncomingCmd(uri)); err != nil {
		return fmt.Errorf("migrate-incoming: %w", err)
	}
	return nil
}

// migrateContinueCmd builds migrate-continue out of the pre-switchover
// state, releasing the source after the disk mirror has finalized.
func migrateContinueCmd() []byte {
	return mustCmd("migrate-continue", map[string]any{"state": "pre-switchover"})
}

// MigrateContinue resumes a paused (pre-switchover) migration (see
// migrateContinueCmd).
func (c *QMPClient) MigrateContinue() error {
	if _, err := c.monitor.Run(migrateContinueCmd()); err != nil {
		return fmt.Errorf("migrate-continue: %w", err)
	}
	return nil
}

// AnnounceParameters tunes a QEMU self-announce: the retry schedule (all
// milliseconds) QEMU uses to repeat the announcement so it survives small frame
// loss. These mirror QEMU's own migration-announce defaults.
type AnnounceParameters struct {
	Initial int // delay before the first announce
	Max     int // cap on the per-round delay
	Rounds  int // number of announcements
	Step    int // per-round delay increment
}

// announceSelfCmd builds announce-self with the given retry schedule.
func announceSelfCmd(p AnnounceParameters) []byte {
	return mustCmd("announce-self", map[string]any{
		"initial": p.Initial,
		"max":     p.Max,
		"rounds":  p.Rounds,
		"step":    p.Step,
	})
}

// AnnounceSelf issues QMP announce-self: QEMU emits self-announcement (RARP,
// MAC-only, IP-independent) frames from each guest NIC's tap so the physical
// switch / learning bridge relearns the MAC on this node's port after a live
// migration. Best-effort at the call site - a missed announce degrades to the
// switch's MAC-aging timeout, never fails the resume.
func (c *QMPClient) AnnounceSelf(p AnnounceParameters) error {
	if _, err := c.monitor.Run(announceSelfCmd(p)); err != nil {
		return fmt.Errorf("announce-self: %w", err)
	}
	return nil
}

// migrateCancelCmd builds migrate_cancel. Note the UNDERSCORE: this is
// the one migration verb QEMU spells with an underscore.
func migrateCancelCmd() []byte {
	return mustCmd("migrate_cancel", nil)
}

// MigrateCancel cancels an in-flight migration (see migrateCancelCmd).
func (c *QMPClient) MigrateCancel() error {
	if _, err := c.monitor.Run(migrateCancelCmd()); err != nil {
		return fmt.Errorf("migrate_cancel: %w", err)
	}
	return nil
}

// queryMigrateCmd builds query-migrate (no arguments).
func queryMigrateCmd() []byte {
	return mustCmd("query-migrate", nil)
}

// QueryMigrate issues query-migrate and returns the decoded status and
// RAM progress counters.
func (c *QMPClient) QueryMigrate() (MigrateInfo, error) {
	raw, err := c.monitor.Run(queryMigrateCmd())
	if err != nil {
		return MigrateInfo{}, fmt.Errorf("query-migrate: %w", err)
	}
	var resp struct {
		Return MigrateInfo `json:"return"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return MigrateInfo{}, fmt.Errorf("query-migrate decode: %w", err)
	}
	return resp.Return, nil
}

// queryBlockJobsCmd builds query-block-jobs (no arguments).
func queryBlockJobsCmd() []byte {
	return mustCmd("query-block-jobs", nil)
}

// QueryBlockJobs issues query-block-jobs and returns the decoded list of
// active block jobs, including the disk mirror's offset/len progress and
// readiness.
func (c *QMPClient) QueryBlockJobs() ([]BlockJobInfo, error) {
	raw, err := c.monitor.Run(queryBlockJobsCmd())
	if err != nil {
		return nil, fmt.Errorf("query-block-jobs: %w", err)
	}
	var resp struct {
		Return []BlockJobInfo `json:"return"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("query-block-jobs decode: %w", err)
	}
	return resp.Return, nil
}

// Events returns the multiplexed QMP event channel for the underlying
// socket monitor. go-qemu fans every asynchronous event out over this
// single channel, so one Events call serves a whole migration run.
func (c *QMPClient) Events(ctx context.Context) (<-chan qmp.Event, error) {
	return c.monitor.Events(ctx)
}

// waitMigrationStatus reads MIGRATION events from ch until one reports
// status==want (returned) or a status listed in failStates is seen
// (returned as an error). A closed channel or a cancelled ctx before
// either is an error - the caller treats that as a failed migration and
// falls back to the source (fail-safe).
func waitMigrationStatus(ctx context.Context, ch <-chan qmp.Event, want string, failStates []string) (string, error) {
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return "", fmt.Errorf("qmp event stream closed before migration status %q", want)
			}
			if ev.Event != "MIGRATION" {
				continue
			}
			st, _ := ev.Data["status"].(string)
			if st == want {
				return st, nil
			}
			for _, f := range failStates {
				if st == f {
					return "", fmt.Errorf("migration entered terminal status %q", st)
				}
			}
		}
	}
}

// waitBlockJobsEvent reads block-job events from ch until eventName has arrived
// for EVERY device id in the set (returned nil). Unlike waitBlockJobEvent it
// must NOT discard an event for a still-pending member of the set: the per-disk
// mirrors reach their event out of order (the tiny cidata mirror reaches
// BLOCK_JOB_READY long before the multi-GiB boot disk), and a single-job waiter
// that discarded the early event would then block on it forever. It crosses off
// each matching id and returns when the set is empty. It fails fast on a job
// error event for a watched device (BLOCK_JOB_ERROR, an errored
// BLOCK_JOB_COMPLETED, or a clean COMPLETED arriving while we still wait for
// READY) rather than blocking until the disk-mirror timeout - a different event
// name than eventName, so the naive matcher would skip it. A closed channel or
// cancelled ctx before the set is satisfied is an error. Events for ids not in
// the set are ignored (there are none in this flow).
func waitBlockJobsEvent(ctx context.Context, ch <-chan qmp.Event, eventName string, remaining map[string]bool) error {
	for len(remaining) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return fmt.Errorf("qmp event stream closed before %s for all jobs", eventName)
			}
			dev, _ := ev.Data["device"].(string)
			if !remaining[dev] {
				continue
			}
			// A failed mirror emits BLOCK_JOB_ERROR (and/or an errored
			// BLOCK_JOB_COMPLETED), a different event name than the one we wait
			// for. Treat any error for a watched device as terminal so a failed
			// mirror aborts fast (fail-safe-to-source) instead of blocking until
			// the disk-mirror timeout.
			if ev.Event == "BLOCK_JOB_ERROR" {
				msg, _ := ev.Data["error"].(string)
				if msg == "" {
					msg = "block job error"
				}
				return fmt.Errorf("block job %q failed before %s: %s", dev, eventName, msg)
			}
			if msg, _ := ev.Data["error"].(string); msg != "" {
				return fmt.Errorf("%s for %q reported error: %s", ev.Event, dev, msg)
			}
			// While waiting for READY, a clean BLOCK_JOB_COMPLETED for a watched
			// job is abnormal (the mirror vanished without becoming READY) - fail
			// closed rather than wait out the deadline.
			if eventName == "BLOCK_JOB_READY" && ev.Event == "BLOCK_JOB_COMPLETED" {
				return fmt.Errorf("block job %q completed before reaching %s", dev, eventName)
			}
			if ev.Event != eventName {
				continue
			}
			delete(remaining, dev)
		}
	}
	return nil
}

// waitBlockJobEvent reads block-job events from ch until eventName arrives
// for device (returned nil). A BLOCK_JOB_COMPLETED / BLOCK_JOB_CANCELLED
// event carrying a non-empty "error" is surfaced as an error. A closed
// channel or cancelled ctx before the event is an error.
func waitBlockJobEvent(ctx context.Context, ch <-chan qmp.Event, eventName, device string) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return fmt.Errorf("qmp event stream closed before %s for %q", eventName, device)
			}
			if dev, _ := ev.Data["device"].(string); dev != device {
				continue
			}
			if ev.Event == eventName {
				if msg, _ := ev.Data["error"].(string); msg != "" {
					return fmt.Errorf("%s for %q reported error: %s", eventName, device, msg)
				}
				return nil
			}
		}
	}
}
