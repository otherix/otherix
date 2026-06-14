// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/migration"
	"github.com/otherix/otherix/internal/agent/qemu"
)

// liveSourceDiskNode is the BlockBackend device name of the OS disk on the
// RUNNING source guest, the device blockdev-mirror copies. The base launch
// (cmdline.go BuildArgs) attaches the boot disk as the FIRST `-drive
// file=...,if=virtio` (cidata is a second if=virtio drive), and QEMU names
// if=virtio BlockBackends virtio0, virtio1, ... in order, so the boot disk's
// device name is virtio0. blockdev-mirror's device param accepts a
// BlockBackend device name, so virtio0 is the stable handle: the boot disk is
// always the first if=virtio drive and the source VM always launches with it
// (never OmitBootDisk). The auto node-name (#blockNNN) is unstable and must
// NOT be hardcoded. Verified via query-block on a running guest.
const liveSourceDiskNode = "virtio0"

// liveTargetDiskNode is the node-name the migration target assigns to its
// destination disk blockdev (LiveIncomingArgs emits it, SetupLiveIncoming
// NBD-exports it, and the booting guest attaches it). Kept stable so the
// export handle and the boot device agree.
const liveTargetDiskNode = "target-disk0"

// liveExportID is the block-export-add handle for the destination disk.
const liveExportID = "exp0"

// liveMirrorJobID is the blockdev-mirror job-id on the source side.
const liveMirrorJobID = "mirror-disk0"

// liveDefaultDowntimeMs is the target max switchover downtime when the spec
// carries none.
const liveDefaultDowntimeMs = 300

// startIncomingLive prepares this node to RECEIVE a live migration: reserve a
// (RAM, NBD) port pair, materialize server TLS creds, create the destination
// disk, adopt a stopped Migrated VM, launch a paused -incoming qemu booting the
// node-named destination disk, and arm the deferred incoming RAM channel + NBD
// disk export over QMP. Synchronous; returns both endpoints + the token so the
// CP can relay them to the source. On any failure after reserving the pair the
// pair is released; no migration record is stored until every step succeeds.
func (m *Manager) startIncomingLive(ctx context.Context, s IncomingSpec) (IncomingResult, error) {
	// Idempotent resume: a re-driven task must not re-reserve ports or
	// re-adopt the VM. Return the endpoints minted the first time.
	if rec, ok := m.migrations.Get(s.MigrationID); ok && rec.Role == migration.RoleTarget {
		nbdEndpoint := net.JoinHostPort(s.BindHost, strconv.Itoa(rec.NBDPort))
		return IncomingResult{ListenEndpoint: rec.ListenEndpt, NBDEndpoint: nbdEndpoint, AuthToken: rec.AuthToken}, nil
	}

	ram, nbd, err := m.migPorts.ReservePair()
	if err != nil {
		// %w so the handler can errors.Is(ErrNoFreePort) and surface a
		// retryable capacity condition rather than a hard failure.
		return IncomingResult{}, fmt.Errorf("reserve migration port pair: %w", err)
	}

	// Full rollback on any pre-success failure: a failed incoming prep must
	// leave NOTHING behind so the CP's retry (re-driving StartIncoming) starts
	// fresh. Each error return below calls cleanup, which unwinds exactly the
	// steps that ran: kill the launched -incoming qemu, drop the adopted VM
	// record + its on-disk state, and release the reserved port pair. adopted
	// and launched track how far execution got.
	var adopted *VM
	launched := false
	cleanup := func() {
		if launched && adopted != nil {
			m.killQEMU(adopted)
		}
		if adopted != nil {
			m.removeAdoptedVM(adopted.ID)
		}
		m.migPorts.ReleasePair(ram, nbd)
	}

	credsDir := filepath.Join(m.stateDir, "migrations", s.MigrationID.String(), "tls")
	if err := qemu.MaterializeMigrationCreds(credsDir, qemu.MigrationTLSServer, m.tlsCA, m.tlsCert, m.tlsKey); err != nil {
		cleanup()
		return IncomingResult{}, err
	}

	v, err := m.AdoptForMigration(AdoptSpec{
		UUID: s.VMUUID, Name: s.VMName, VCPUs: s.VCPUs, MemoryMB: s.MemoryMB,
		PoolName: s.PoolName, Architecture: s.Architecture,
		InitialStatus: StatusMigratingIncoming,
	})
	if err != nil {
		cleanup()
		return IncomingResult{}, err
	}
	adopted = v

	// Destination disk virtual size must be >= the source's (see the offline
	// StartIncoming for the full rationale): max(DiskSizeBytes, ExpectedSize).
	virtualBytes := s.DiskSizeBytes
	if s.ExpectedSize > virtualBytes {
		virtualBytes = s.ExpectedSize
	}
	if err := m.migCreateDisk(ctx, v.DiskPath, virtualBytes); err != nil {
		cleanup()
		return IncomingResult{}, err
	}

	token := s.MigrationID.String()
	ls := qemu.LiveIncomingSpec{
		CredsDir:       credsDir,
		SourceIdentity: s.SourceIdentity,
		DiskNode:       liveTargetDiskNode,
		DiskPath:       v.DiskPath,
		Export:         token,
		ExportID:       liveExportID,
		BindHost:       s.BindHost,
		NBDPort:        nbd,
		RAMPort:        ram,
	}

	// Launch the paused -incoming qemu, then dial it and arm the incoming
	// transport (NBD export + RAM channel) over QMP. The launched qemu keeps
	// running; the setup conn is closed once setup completes (it only carries
	// the monitoring socket, not the qemu process).
	if err := m.migLaunchIncoming(ctx, v, ls); err != nil {
		cleanup()
		return IncomingResult{}, fmt.Errorf("launch incoming qemu: %v", err)
	}
	launched = true
	conn, err := m.migDialQMP(v.QMPSocket)
	if err != nil {
		cleanup()
		return IncomingResult{}, fmt.Errorf("dial target qmp: %v", err)
	}
	// Closing the QMP client closes only the monitoring socket, NOT the
	// launched qemu process, so it is safe to close after setup.
	defer func() { _ = conn.Close() }()
	if err := qemu.SetupLiveIncoming(conn, ls); err != nil {
		cleanup()
		return IncomingResult{}, fmt.Errorf("setup live incoming: %v", err)
	}

	ramEndpoint := net.JoinHostPort(s.BindHost, strconv.Itoa(ram))
	nbdEndpoint := net.JoinHostPort(s.BindHost, strconv.Itoa(nbd))
	m.migrations.Put(&migration.Record{
		MigrationID: s.MigrationID, VMID: s.VMUUID, VMName: s.VMName,
		Role: migration.RoleTarget, Mode: migration.ModeLive, Phase: migration.PhaseSetup,
		Port: ram, NBDPort: nbd, ListenEndpt: ramEndpoint, AuthToken: token,
		CredsDir: credsDir, CreatedAt: time.Now().UTC(),
	})
	return IncomingResult{ListenEndpoint: ramEndpoint, NBDEndpoint: nbdEndpoint, AuthToken: token}, nil
}

// runOutgoingLive drives the SOURCE side of a live migration asynchronously.
// Unlike the offline push it does NOT power off the guest: a live migration
// moves the running guest, finalizing the disk mirror while paused at
// pre-switchover and only then releasing the guest to the target (the order
// RunLiveSource enforces). On success the guest now runs on the target; on any
// failure RunLiveSource aborts both the RAM stream and the disk mirror and the
// guest stays running on the source.
func (m *Manager) runOutgoingLive(ctx context.Context, taskID uuid.UUID, s OutgoingSpec) {
	fail := func(code, msg string) {
		m.migrations.Update(s.MigrationID, func(r *migration.Record) {
			r.Phase = migration.PhaseFailed
			r.ErrorMessage = msg
			r.CompletedAt = time.Now().UTC()
		})
		m.tasks.Update(taskID, func(t *AgentTask) {
			t.Status = TaskStatusFailed
			t.Error = &TaskError{Code: code, Message: msg}
		})
	}

	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	v, err := m.Get(s.VMUUID)
	if err != nil {
		fail("internal", fmt.Sprintf("vm %s: %v", s.VMUUID, err))
		return
	}
	m.migrations.Update(s.MigrationID, func(r *migration.Record) {
		r.Phase = migration.PhaseActive
		r.StartedAt = time.Now().UTC()
		r.BlockJobID = liveMirrorJobID
	})

	credsDir := filepath.Join(m.stateDir, "migrations", s.MigrationID.String(), "tls")
	if err := qemu.MaterializeMigrationCreds(credsDir, qemu.MigrationTLSClient, m.tlsCA, m.tlsCert, m.tlsKey); err != nil {
		fail("tls_setup_failed", err.Error())
		return
	}
	m.migrations.Update(s.MigrationID, func(r *migration.Record) { r.CredsDir = credsDir })

	ramHost, ramPortStr, err := net.SplitHostPort(s.TargetEndpoint)
	if err != nil {
		fail("internal", fmt.Sprintf("bad target endpoint %q: %v", s.TargetEndpoint, err))
		return
	}
	ramPort, _ := strconv.Atoi(ramPortStr)
	nbdHost, nbdPortStr, err := net.SplitHostPort(s.NBDEndpoint)
	if err != nil {
		fail("internal", fmt.Sprintf("bad nbd endpoint %q: %v", s.NBDEndpoint, err))
		return
	}
	nbdPort, _ := strconv.Atoi(nbdPortStr)

	conn, err := m.migDialQMP(v.QMPSocket)
	if err != nil {
		fail("qmp_unavailable", fmt.Sprintf("dial source qmp: %v", err))
		return
	}
	// The manager owns the dialed conn; close the monitoring socket on return
	// (closing it does not affect the running guest). RunLiveSource borrows it
	// but never owns it.
	defer func() { _ = conn.Close() }()

	spec := qemu.LiveSourceSpec{
		SrcDiskNode:          liveSourceDiskNode,
		JobID:                liveMirrorJobID,
		NBDNode:              "mirror-target",
		TargetHost:           nbdHost,
		NBDPort:              nbdPort,
		Export:               s.AuthToken,
		CredsDir:             credsDir,
		TargetIdentity:       s.TargetIdentity,
		RAMHost:              ramHost,
		RAMPort:              ramPort,
		DowntimeMs:           liveDefaultDowntimeMs,
		ConvergenceTimeout:   m.migConvergenceTimeout,
		ProgressPollInterval: 2 * time.Second,
	}

	runErr := m.migRunLiveSource(ctx, conn, spec, m.liveProgressReporter(s.MigrationID))
	if runErr != nil {
		fail("convergence_failed", fmt.Sprintf("live migration: %v", runErr))
		return
	}

	m.migrations.Update(s.MigrationID, func(r *migration.Record) {
		r.Phase = migration.PhaseCompleted
		r.CompletedAt = time.Now().UTC()
	})
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusSuccess })
}

// liveProgressReporter returns the periodic LiveProgress callback handed to
// RunLiveSource. Each tick it (1) logs the full disk-mirror + RAM detail at
// INFO so an operator (and we) can SEE exactly where a migration is stuck, and
// (2) updates the coarse bytes-progress on the migration record so
// `migration get` reflects it. During the DISK phase (no/empty RAM-migrate
// status, a mirror block job present) the bytes come from the boot disk's
// block job offset/len; once the RAM phase has started (migrate status
// non-empty and not "none") they come from the query-migrate RAM counters.
//
// NOTE: surfacing the FULL query-migrate breakdown through the CP
// `migration get` API (new Migration schema fields) is a deliberate follow-up,
// out of scope here - this puts the full detail in the AGENT LOG and the
// coarse bytes-progress on the existing record/API.
func (m *Manager) liveProgressReporter(migrationID uuid.UUID) func(qemu.LiveProgress) {
	return func(p qemu.LiveProgress) {
		bootJob, hasBootJob := liveBootDiskJob(p.BlockJobs)
		ramStarted := p.Migrate.Status != "" && p.Migrate.Status != "none"

		log := m.log.With("migration_id", migrationID.String())
		if hasBootJob {
			log = log.With(
				"block_job_device", bootJob.Device,
				"block_job_offset", bootJob.Offset,
				"block_job_len", bootJob.Len,
				"block_job_ready", bootJob.Ready,
				"block_job_status", bootJob.Status,
			)
		} else {
			log = log.With("block_jobs", len(p.BlockJobs))
		}
		log.Info("live migration progress",
			"migrate_status", p.Migrate.Status,
			"ram_remaining", p.Migrate.RAM.Remaining,
			"ram_transferred", p.Migrate.RAM.Transferred,
			"ram_total", p.Migrate.RAM.Total,
			"ram_dirty_pages_rate", p.Migrate.RAM.DirtyPagesRate,
		)

		m.migrations.Update(migrationID, func(r *migration.Record) {
			switch {
			case ramStarted:
				r.BytesTotal = p.Migrate.RAM.Total
				r.BytesTransferred = p.Migrate.RAM.Transferred
			case hasBootJob:
				r.BytesTotal = bootJob.Len
				r.BytesTransferred = bootJob.Offset
			}
		})
	}
}

// liveBootDiskJob returns the block job mirroring the boot disk
// (Device == liveSourceDiskNode) and whether it was found. The reporter keys
// disk-phase progress on this entry; an absent entry means the mirror has not
// started (or has finalized) and the reporter falls back to logging the raw
// count.
func liveBootDiskJob(jobs []qemu.BlockJobInfo) (qemu.BlockJobInfo, bool) {
	for _, j := range jobs {
		if j.Device == liveSourceDiskNode {
			return j, true
		}
	}
	return qemu.BlockJobInfo{}, false
}

// cancelLive aborts a pre-cutover live migration best-effort: cancel the RAM
// migration and the disk mirror on the source guest, release the reserved
// ports, and pin phase=cancelled. Idempotent; a terminal record is returned
// unchanged. A failure to obtain a QMP conn does not block the bookkeeping -
// the ports are still released and the phase still set so the slot frees.
func (m *Manager) cancelLive(id uuid.UUID, rec migration.Record) (MigrationView, bool) {
	if rec.Terminal() {
		return m.GetMigration(id)
	}

	// Best-effort QMP abort. Only the source holds the live mirror + RAM
	// stream; the target side is reclaimed by the CP re-driving as failed.
	if rec.Role == migration.RoleSource {
		if v, err := m.Get(rec.VMID); err == nil {
			if conn, err := m.migDialQMP(v.QMPSocket); err == nil {
				_ = conn.MigrateCancel()
				if rec.BlockJobID != "" {
					_ = conn.BlockJobCancel(rec.BlockJobID, true)
				}
			}
		}
	}

	if rec.Port > 0 || rec.NBDPort > 0 {
		m.migPorts.ReleasePair(rec.Port, rec.NBDPort)
	}
	m.migrations.Update(id, func(r *migration.Record) {
		r.Phase = migration.PhaseCancelled
		r.ErrorMessage = "cancelled"
		r.CompletedAt = time.Now().UTC()
	})
	return m.GetMigration(id)
}

// launchIncomingQemu is the production migLaunchIncoming: it boots a paused
// -incoming qemu for the adopted target VM v, booting the node-named
// destination disk (so SetupLiveIncoming can NBD-export it) instead of the base
// `-drive`, and returns once the QMP socket answers. The base args omit the OS
// disk drive (OmitBootDisk) to avoid expressing the boot disk twice; the
// node-named blockdev from LiveIncomingArgs plus an explicit virtio-blk device
// supply it. SMOKE MUST VALIDATE the resulting cmdline boots and the export +
// boot device agree on liveTargetDiskNode.
func (m *Manager) launchIncomingQemu(ctx context.Context, v *VM, ls qemu.LiveIncomingSpec) error {
	binary, err := qemu.Binary(v.Architecture)
	if err != nil {
		return fmt.Errorf("qemu binary: %v", err)
	}
	args, err := qemu.BuildArgs(qemu.VMSpec{
		Name:            v.Name,
		UUID:            v.ID,
		VCPUs:           v.VCPUs,
		MemoryMB:        v.MemoryMB,
		Architecture:    v.Architecture,
		Accelerator:     m.accelerator,
		DiskPath:        v.DiskPath,
		QMPSocket:       v.QMPSocket,
		ConsoleSocket:   v.ConsoleSocket,
		PIDFile:         v.PIDFile,
		AArch64Firmware: m.aarch64Firmware,
		NICs:            v.NICs,
		OmitBootDisk:    true,
	})
	if err != nil {
		return fmt.Errorf("build qemu args: %v", err)
	}
	// Append the migration disk as a node-named blockdev + boot device, and
	// -incoming defer (LiveIncomingArgs supplies both blockdev and -incoming).
	args = append(args, qemu.LiveIncomingArgs(ls)...)
	args = append(args, "-device", fmt.Sprintf("virtio-blk-pci,drive=%s", ls.DiskNode))

	spawnCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := qemu.Spawn(spawnCtx, binary, args); err != nil {
		return fmt.Errorf("spawn incoming qemu: %v", err)
	}

	// Wait until QMP answers so SetupLiveIncoming can issue commands.
	deadline := time.Now().Add(15 * time.Second)
	for {
		c, derr := qemu.DialQMP(v.QMPSocket, 2*time.Second)
		if derr == nil {
			_ = c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("incoming qemu qmp not reachable: %v", derr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
