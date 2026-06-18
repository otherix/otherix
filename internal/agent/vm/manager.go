// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/agent/cloudinit"
	"github.com/otherix/otherix/internal/agent/migration"
	"github.com/otherix/otherix/internal/agent/netfabric"
	"github.com/otherix/otherix/internal/agent/qemu"
	"github.com/otherix/otherix/internal/agent/serialmux"
	"github.com/otherix/otherix/internal/agent/state"
	"github.com/otherix/otherix/internal/agent/storage"
	"github.com/otherix/otherix/internal/config"
)

// Errors returned by Manager methods. Sentinel errors so handlers can
// branch on errors.Is for envelope mapping.
var (
	ErrNotFound    = errors.New("vm not found")
	ErrPoolUnknown = errors.New("pool name does not match a configured pool")
	ErrInvalidSpec = errors.New("invalid create spec")

	// ErrInvalidState is returned by sync lifecycle ops (Pause /
	// Resume / Reset) when the VM is not in a phase that accepts
	// the requested transition (e.g. resume-when-running,
	// pause-when-stopped). Handlers map this to 409 conflict.
	ErrInvalidState = errors.New("vm not in a valid state for this operation")

	// ErrQMPUnavailable wraps a failed QMP socket dial or command
	// dispatch. Handlers map this to 500 — the operator can re-
	// invoke the action; the agent does not retry automatically
	// (Area 4-IV lock).
	ErrQMPUnavailable = errors.New("qmp socket unavailable or rejected command")

	// ErrInFlight is returned by async lifecycle entrypoints when the
	// same VM (by name) already has an operation in progress. Locked
	// via inFlight sync.Map[name]struct{} per L3 D2: prevents the VM
	// reconciler from racing concurrent Start / Stop / Poweroff /
	// Reboot / Delete on the same VM. Handlers map this to 409
	// conflict; the reconciler reads HasInFlight before enqueuing
	// corrective ops to short-circuit without the sentinel round-trip.
	ErrInFlight = errors.New("vm operation already in flight")

	// ErrCreateInFlight is returned by Create on a duplicate vmID whose
	// original create task record exists but whose task is no longer
	// retrievable. It is a defensive fallback for a should-not-happen state
	// (the task store keeps records for the process lifetime); handlers map
	// it to 409 conflict so the CP worker resumes on its next poll rather
	// than the agent clobbering the live VM.
	ErrCreateInFlight = errors.New("vm create already in flight for this id")
)

// shutdownGrace bounds how long Delete and Stop wait for system_powerdown
// to take effect. For Stop the timeout is a task-failure trigger (Area
// 4-II lock — graceful only; operator dispatches to poweroff via CLI
// --force on timeout). For Delete the timeout cascades through Quit /
// SIGKILL.
const shutdownGrace = 60 * time.Second

// poweroffGrace bounds how long Poweroff waits for QMP `quit` to take
// effect before resorting to SIGKILL. Short by design — poweroff is the
// force path; operators reach for it when graceful shutdown would not
// suffice.
const poweroffGrace = 5 * time.Second

// pool is the agent's name-keyed view of one storage pool. The
// registry is name-keyed; the cluster-wide UUID stays at the CP
// edge's storage-pool carve-out.
type pool struct {
	name string
	root string
}

// Manager is the agent-side VM lifecycle orchestrator. One instance per
// agent process. All state is in-memory; Manager.New rebuilds it from
// disk on startup.
//
// The pool registry has its own RWMutex distinct from `mu` (which
// serialises VM-state changes). Pool churn driven by the reconciler
// must not block VM ops, and VM op pool lookups want only a read lock.
type Manager struct {
	log             *slog.Logger
	stateDir        string
	aarch64Firmware string
	accelerator     string
	fabric          netfabric.Fabric

	mu    sync.Mutex
	vms   map[uuid.UUID]*VM
	tasks *TaskStore

	// createTasks maps a VM id to the id of the vm.create task that first
	// materialised it. Create consults it to re-accept the original task on
	// a duplicate POST (redelivery before the CP persisted the
	// agent_task_id) rather than clobbering the in-flight VM. Written under
	// mu alongside vms; an entry is set when Create spawns runCreate and is
	// never removed (the task store keeps the record for the process
	// lifetime, and Delete tears the VM down by name, not via this map).
	createTasks map[uuid.UUID]uuid.UUID

	poolsMu sync.RWMutex
	pools   map[string]pool

	// imageDialControl is the net.Dialer Control hook applied to every dial
	// the image download client performs, including redirect hops (the
	// Control runs on the resolved IP, post-DNS, so it is DNS-rebind-safe).
	// New sets it to blockLocalDial (SSRF guard, audit M1); image tests
	// driving httptest servers on loopback set it to nil to opt out.
	imageDialControl func(network, address string, c syscall.RawConn) error

	// imageLocks serialises concurrent storage-image work on the same
	// (pool, basename). Keys are imageLockKey, values are *sync.Mutex.
	// The map grows monotonically — bounded by active distinct basenames
	// the operator imports. Cleanup is a future iteration concern (see
	// ROADMAP). sync.Map's zero value is usable so no init is needed.
	imageLocks sync.Map

	// inFlight tracks in-progress lifecycle operations keyed by VM
	// name. Entry value is struct{}; presence alone signals "busy".
	// Loaded into via LoadOrStore at Start / Stop / Poweroff /
	// Reboot / Delete entry; Deleted by the goroutine on completion
	// (regardless of success / fail). HasInFlight surfaces the
	// state to the reconciler so it can skip enqueuing duplicates.
	inFlight sync.Map

	// muxes maps a running VM's name to its serial console
	// multiplexer (see ADR 0029). An entry exists from the moment
	// Start finishes attachMux until Delete tears it down. Stop /
	// Poweroff intentionally do not remove the entry: the multiplexer
	// keeps the on-disk log file readable until Delete removes the
	// VM's state directory (L17). Agent restart leaves entries empty
	// even for VMs that survived in-place; a subsequent reboot
	// re-attaches via reconnectMux's fallback.
	muxesMu sync.Mutex
	muxes   map[string]*serialmux.Multiplexer

	// migrations holds the agent's in-memory migration records (target
	// and source side); migPorts hands out ADR 0013 ingress ports for
	// incoming migrations. Neither is persisted - the CP re-drives on
	// agent restart.
	migrations *migration.Store
	migPorts   *migration.PortAllocator

	// tlsCA / tlsCert / tlsKey are the agent's mTLS material paths
	// (cfg.TLS.*), reused to materialise per-migration qemu-nbd server
	// creds (Task 4).
	tlsCA, tlsCert, tlsKey string

	// QEMU side-effect seams for the migration path, overridable in
	// tests. Default to the real qemu.* helpers in New.
	migCreateDisk    func(ctx context.Context, path string, virtualBytes int64) error
	migCreateRawDisk func(ctx context.Context, path string, virtualBytes int64) error
	// migBuildCidata rebuilds the read-only cidata ISO locally from the
	// migrated VM's cloud-init inputs (a read-only disk cannot be live
	// block-mirrored). cidata is deterministic and read once at boot, so the
	// target reconstructs it instead of NBD-exporting + mirroring it. Defaults
	// to the cloudinit.Builder in New; stubbed in tests.
	migBuildCidata func(path, hostname string, userData, networkData []byte) error
	// createBuildCidata builds the always-on NoCloud seed during runCreate.
	// Same shape as migBuildCidata so both paths share the cloudinit.Builder
	// wiring; split into its own seam so a create/recreate test can spy the
	// hostname passed without a real ISO build. Defaults to the
	// cloudinit.Builder in New.
	createBuildCidata func(path, hostname string, userData, networkData []byte) error
	migSpawnNBD       func(ctx context.Context, args []string) (int, error)
	migRunConvert     func(ctx context.Context, args []string) error
	migWaitNBDReady   func(ctx context.Context, endpoint string) error

	// Live-migration seams. migLaunchIncoming boots a paused -incoming
	// qemu for an adopted target VM and waits until its QMP socket is
	// reachable; migDialQMP dials a VM's QMP socket as a LiveSourceConn;
	// migRunLiveSource drives the source sequencer. Default to the real
	// impls in New; tests inject no-op / recording fakes.
	migLaunchIncoming func(ctx context.Context, v *VM, ls qemu.LiveIncomingSpec) error
	migDialQMP        func(socket string) (qemu.LiveSourceConn, error)
	migRunLiveSource  func(ctx context.Context, conn qemu.LiveSourceConn, spec qemu.LiveSourceSpec, report func(qemu.LiveProgress)) error

	// Target-side resume seams. migDialQMPTarget dials a paused -incoming
	// qemu's QMP socket as a LiveTargetConn; migRunLiveTarget drives the
	// target sequencer (wait incoming completed -> drop the writable export
	// -> cont). Default to the real impls in New; tests inject stub conns and
	// a recording run.
	migDialQMPTarget func(socket string) (qemu.LiveTargetConn, error)
	migRunLiveTarget func(ctx context.Context, conn qemu.LiveTargetConn, spec qemu.LiveTargetSpec) error

	// resumeWG tracks the fire-and-forget runIncomingResume goroutines
	// startIncomingLive spawns (each detached on context.Background so it
	// outlives the 202 request). Production never waits on it; tests Wait at
	// teardown so a resume's persistVM write cannot race t.TempDir cleanup.
	resumeWG sync.WaitGroup

	// migConvergenceTimeout bounds the live-migration RAM watchdog. Set
	// from cfg.Migration.ConvergenceTimeout in New, with a non-zero guard
	// (a zero timeout would make the watchdog fire instantly).
	migConvergenceTimeout time.Duration

	// Recovery seams for reconcileRecoveredIncoming (orphaned
	// StatusMigratingIncoming VMs replayed at boot). probeIncoming reads the
	// VM's pidfile and (if alive) QMP-dials to read its run-state; killIncoming
	// terminates the leaked -incoming qemu. Both default to the real impls in
	// New; tests inject recording / scripted fakes. They are wired into a
	// Manager field rather than called directly so a recovery test can drive
	// the three-way branch (running / paused / dead) without a real qemu - the
	// replay loop runs inside New, so a post-construction field assignment
	// cannot reach it; New copies the package-level probeRecoveredIncoming hook
	// here BEFORE the replay loop.
	probeIncoming func(qmpSocket, pidFile string) incomingProbe
	killIncoming  func(v *VM)

	// diskCapturer captures one VM disk device into a destination backup
	// file. Production dials the VM's QMP socket and runs qemu
	// blockdev-backup (qmpDiskCapturer); the snapshot manager test injects a
	// fake that copies a fixture qcow2 so CreateSnapshot is unit-testable on
	// darwin without real qemu. The seam mirrors the migration path's
	// migDialQMP / migRunLiveSource injection.
	diskCapturer diskCapturer

	// snapshotConvert materializes a standalone qcow2 copy of a captured
	// backup into a content-addressed blob (produceBlob's convertFunc seam).
	// Production is qemu.ConvertTo; the snapshot manager test injects a
	// verbatim copy so the capture path runs on darwin without qemu-img.
	snapshotConvert convertFunc

	// snapshotDiskDevices enumerates a VM's disks in virtio-index order for
	// the snapshot capture loop. Production is the package snapshotDiskDevices
	// (slice A: a single boot disk at index 0); the test injects a multi-disk
	// fixture to pin the manifest's deterministic index ordering. The field is
	// a test-only seam: slice A's enumerator cannot yield 2 disks, so there is
	// no other way to drive runSnapshotCreate's index sort.
	snapshotDiskDevices func(*VM) []snapshotDiskDevice

	// snapshotInFlight serialises concurrent snapshot captures on the same
	// (vmName, snapshotName): a redelivered create whose capture is still
	// running returns the original *AgentTask instead of starting a second
	// blockdev-backup. Keyed by snapshotInFlightKey, value *AgentTask. Cleared
	// by the capture goroutine on completion. The on-disk manifest closes the
	// window after the task settles; this map closes it while it runs.
	snapshotInFlightMu sync.Mutex
	snapshotInFlight   map[snapshotInFlightKey]*AgentTask
}

// snapshotInFlightKey identifies an in-progress snapshot capture by the VM
// name and the snapshot name (the agent's defense-in-depth idempotency key
// closing the CP-side worker-redelivery residual window).
type snapshotInFlightKey struct {
	vmName       string
	snapshotName string
}

// incomingProbe is the result of probing a replayed StatusMigratingIncoming
// VM's qemu during recovery. It is the fail-closed input to the kill decision
// in reconcileRecoveredIncoming: the qemu is killed ONLY on a positive
// pre-resume run-state (alive && queried && preResumeIncomingRunStates, e.g.
// inmigrate/paused); every other shape (running -> promote; dead -> mark failed,
// nothing to kill; dial/query failure or an unexpected run-state -> inconclusive,
// no kill) must NOT kill.
type incomingProbe struct {
	// alive is true when the pidfile parsed and the process answers signal 0.
	alive bool
	// queried is true when the QMP socket dial AND query-status both succeeded
	// (only meaningful when alive). It is false if the dial failed OR query-status
	// errored even though the socket dialed; a false queried with alive=true marks
	// an inconclusive probe.
	queried bool
	// runState is the qemu query-status run-state ("running", "paused", ...),
	// set only when queried is true.
	runState string
}

// probeRecoveredIncoming is the production probe for a replayed
// StatusMigratingIncoming VM. It reads the pidfile, checks liveness, and (when
// alive) dials QMP to read the run-state. Package-level so recovery tests can
// substitute a scripted probe before calling New (the replay loop that consults
// it runs inside New, out of reach of a post-construction field assignment).
var probeRecoveredIncoming = func(qmpSocket, pidFile string) incomingProbe {
	pid, err := qemu.ReadPIDFile(pidFile)
	if err != nil || pid <= 0 || !qemu.IsAlive(pid) {
		return incomingProbe{alive: false}
	}
	conn, err := qemu.DialQMP(qmpSocket, 5*time.Second)
	if err != nil {
		// Process alive but its QMP socket is unreachable: inconclusive. The
		// caller must NOT kill on this - it lacks a positive paused signal.
		return incomingProbe{alive: true, queried: false}
	}
	defer func() { _ = conn.Close() }()
	runState, err := conn.QueryStatus()
	if err != nil {
		return incomingProbe{alive: true, queried: false}
	}
	return incomingProbe{alive: true, queried: true, runState: runState}
}

// killRecoveredIncoming terminates the leaked pre-resume -incoming qemu of a
// recovered orphaned target. Package-level so a recovery test can spy the kill
// before calling New (the replay loop binds m.killIncoming to it). Defaults to
// the manager's best-effort killQEMU.
var killRecoveredIncoming = func(m *Manager, v *VM) { m.killQEMU(v) }

// inFlightAcquire records a new in-flight operation for name. Returns
// (release, ok) — ok=true when the slot was free and the caller may
// proceed (must call release when the goroutine ends); ok=false when
// a prior operation is still resident, in which case the caller must
// reject with ErrInFlight without spawning work.
func (m *Manager) inFlightAcquire(name string) (release func(), ok bool) {
	if name == "" {
		return func() {}, true
	}
	if _, loaded := m.inFlight.LoadOrStore(name, struct{}{}); loaded {
		return nil, false
	}
	return func() { m.inFlight.Delete(name) }, true
}

// HasInFlight reports whether a lifecycle operation is currently
// running on name. Read-only — does not register a new operation. The
// VM reconciler uses this to skip diff-driven corrections when the
// previous tick's enqueue is still in progress.
func (m *Manager) HasInFlight(name string) bool {
	if name == "" {
		return false
	}
	_, ok := m.inFlight.Load(name)
	return ok
}

// PoolView is the read-only projection of a registered pool. Consumed
// by the reconciler's diff loop AND by future external introspection
// (e.g. an admin endpoint listing locally-materialised pools). The
// internal `pool` struct stays unexported.
type PoolView struct {
	Name string
	Root string
}

// New constructs a Manager. The fabric materialises and tears down the
// host-side network primitives (taps) for VM NICs; production passes
// netfabric.New, tests inject a FakeFabric. The pool registry starts
// **empty** — the reconciler populates it from desired-state delivered
// through heartbeat. The constructor only ensures the state directory and
// replays existing meta.json files. VM ops that reference a pool not
// yet reconciled return ErrPoolUnknown until the reconciler lands the
// entry; the first heartbeat tick typically closes the window.
//
// Probing live qemu processes happens lazily on Get / List rather
// than during startup so transient probe failures do not block agent
// boot.
func New(cfg *config.AgentConfig, fabric netfabric.Fabric, log *slog.Logger) (*Manager, error) {
	if cfg.StatePath == "" {
		return nil, fmt.Errorf("state_path is required")
	}
	if err := ensureWritableDir(cfg.StatePath); err != nil {
		return nil, fmt.Errorf("state_path: %w", err)
	}

	accelerator := qemu.DetectAccelerator()
	log.Info("qemu accelerator selected", "accelerator", accelerator)

	m := &Manager{
		log:              log,
		stateDir:         cfg.StatePath,
		pools:            map[string]pool{},
		aarch64Firmware:  cfg.QEMU.AArch64FirmwarePath,
		accelerator:      accelerator,
		fabric:           fabric,
		imageDialControl: blockLocalDial,
		vms:              map[uuid.UUID]*VM{},
		tasks:            NewTaskStore(),
		createTasks:      map[uuid.UUID]uuid.UUID{},
		muxes:            map[string]*serialmux.Multiplexer{},
		snapshotInFlight: map[snapshotInFlightKey]*AgentTask{},
	}
	m.diskCapturer = qmpDiskCapturer{}
	m.snapshotConvert = qemu.ConvertTo
	m.snapshotDiskDevices = snapshotDiskDevices
	m.migrations = migration.NewStore()
	m.migPorts = migration.NewPortAllocator(cfg.Migration.PortRangeStart, cfg.Migration.PortRangeEnd)
	m.tlsCA, m.tlsCert, m.tlsKey = cfg.TLS.CACertPath, cfg.TLS.CertPath, cfg.TLS.KeyPath
	m.migCreateDisk = qemu.CreateDisk
	m.migCreateRawDisk = qemu.CreateRawDisk
	m.migBuildCidata = func(path, hostname string, userData, networkData []byte) error {
		b := &cloudinit.Builder{Hostname: hostname, UserData: userData, NetworkData: networkData}
		_, err := b.Build(path)
		return err
	}
	m.createBuildCidata = func(path, hostname string, userData, networkData []byte) error {
		b := &cloudinit.Builder{Hostname: hostname, UserData: userData, NetworkData: networkData}
		_, err := b.Build(path)
		return err
	}
	m.migSpawnNBD = qemu.SpawnQemuNBD
	m.migRunConvert = qemu.RunQemuImgConvert
	m.migWaitNBDReady = func(ctx context.Context, endpoint string) error {
		return qemu.WaitNBDListening(ctx, endpoint, 15*time.Second)
	}
	m.migDialQMP = func(socket string) (qemu.LiveSourceConn, error) {
		return qemu.DialQMP(socket, 5*time.Second)
	}
	m.migLaunchIncoming = m.launchIncomingQemu
	m.migRunLiveSource = qemu.RunLiveSource
	m.migDialQMPTarget = func(socket string) (qemu.LiveTargetConn, error) {
		return qemu.DialQMP(socket, 5*time.Second)
	}
	m.migRunLiveTarget = qemu.RunLiveTarget
	m.migConvergenceTimeout = cfg.Migration.ConvergenceTimeout
	if m.migConvergenceTimeout <= 0 {
		m.migConvergenceTimeout = 10 * time.Minute
	}
	// Recovery seams. probeIncoming and killIncoming read package-level hooks so
	// a recovery test can script the three-way branch and spy the kill before
	// New runs the replay loop (which consults them); both default to the real
	// impls. Set BEFORE the replay loop - reconcileRecoveredIncoming runs inside
	// it.
	m.probeIncoming = probeRecoveredIncoming
	m.killIncoming = func(v *VM) { killRecoveredIncoming(m, v) }

	metas, err := state.ScanState(cfg.StatePath, log)
	if err != nil {
		return nil, fmt.Errorf("scan state: %w", err)
	}
	for _, meta := range metas {
		v := &VM{
			ID:            meta.VMID,
			Name:          meta.Name,
			VCPUs:         meta.VCPUs,
			MemoryMB:      meta.MemoryMB,
			PoolName:      meta.PoolName,
			Architecture:  qemu.Architecture(meta.Architecture),
			Status:        Status(meta.Status),
			CreatedAt:     meta.CreatedAt,
			UpdatedAt:     meta.UpdatedAt,
			DiskPath:      meta.DiskPath,
			QMPSocket:     meta.QMPSocket,
			ConsoleSocket: meta.ConsoleSocket,
			PIDFile:       meta.PIDFile,
			CidataPath:    meta.CidataPath,
			NICs:          metaToNICs(meta.NICs),
			Migrated:      meta.Migrated,
		}
		m.vms[v.ID] = v

		// Re-attach the serial multiplexer for a VM that is still running
		// so `vm console` / `vm logs` survive an agent restart: the qemu
		// process and its console socket outlive the agent, so a fresh
		// agent must reconnect to them - otherwise GetMux returns nil and
		// both endpoints report "restart the vm to re-enable" until the VM
		// itself is restarted. Best-effort: a dial failure (qemu actually
		// gone since meta.json was written) is logged and never blocks
		// boot - the VM is probed lazily on Get/List and its true state is
		// reported through heartbeat. Stopped VMs have no console socket.
		switch v.Status {
		case StatusRunning:
			if err := m.attachMux(log, v); err != nil {
				log.Warn("recover: re-attach serial multiplexer",
					"vm", v.Name, "err", err.Error())
			}
		case StatusMigratingIncoming:
			// An orphaned live-migration target replayed with its transitional
			// status: the in-memory migration record, resume goroutine, and mux
			// did not survive the restart, so the VM reconciler would wedge it in
			// `migrating` forever and leak the paused -incoming qemu (RAM + NBD
			// ingress ports held by its in-process exports). Promote it if a
			// post-cutover guest is actually running, else reap the leak - but
			// NEVER delete the destination disk (a post-cutover crash makes it the
			// only copy).
			m.reconcileRecoveredIncoming(log, v)
		}
	}

	// Reclaim host taps left behind by VMs that no longer exist (crash
	// before teardown, or a meta.json skipped during replay). Runs once
	// here, with the replayed-VM set authoritative; best-effort, never
	// fails startup.
	m.sweepOrphanTaps()

	log.Info("vm manager initialized",
		"state_dir", cfg.StatePath,
		"recovered_vms", len(m.vms),
		"note", "pool registry empty; reconciler populates via heartbeat",
	)
	return m, nil
}

// AddPool registers a pool in the in-memory registry. The reconciler
// invokes this when a declared_pools entry arrives that the agent
// has not yet materialised. The function is idempotent - re-adding
// an identical (name, root) pair is a no-op; re-adding with a
// different root replaces the registry entry but does **not** move
// existing files on disk (operator responsibility).
//
// Filesystem-side: ensures root and the conventional pool subdirs
// (images/, scratch/import/) exist and are writable. Errors here
// propagate up to the reconciler, which records a `failed` outcome on
// the next heartbeat.
func (m *Manager) AddPool(name, root string) error {
	if name == "" {
		return fmt.Errorf("pool: name is required")
	}
	if root == "" {
		return fmt.Errorf("pool %q: root is required", name)
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("pool %q: root must be absolute (got %q)", name, root)
	}
	if err := ensureWritableDir(root); err != nil {
		return fmt.Errorf("pool %q root: %w", name, err)
	}
	if err := ensurePoolSubdirs(root); err != nil {
		return fmt.Errorf("pool %q subdirs: %w", name, err)
	}
	m.poolsMu.Lock()
	m.pools[name] = pool{name: name, root: root}
	m.poolsMu.Unlock()
	return nil
}

// RemovePool drops a pool from the in-memory registry. Per SL11 the
// filesystem is left intact — operator responsibility to reclaim
// space if desired. Subsequent VM ops referencing this pool name
// return ErrPoolUnknown.
func (m *Manager) RemovePool(name string) {
	m.poolsMu.Lock()
	delete(m.pools, name)
	m.poolsMu.Unlock()
}

// ListPools returns a snapshot of the registered pools. Order is not
// guaranteed (map iteration); callers that want deterministic order
// sort by Name. Read-only — mutations require AddPool / RemovePool.
func (m *Manager) ListPools() []PoolView {
	m.poolsMu.RLock()
	defer m.poolsMu.RUnlock()
	out := make([]PoolView, 0, len(m.pools))
	for _, p := range m.pools {
		out = append(out, PoolView{Name: p.name, Root: p.root})
	}
	return out
}

func ensureWritableDir(path string) error {
	if err := os.MkdirAll(path, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	probe := filepath.Join(path, ".otherix-write-probe")
	if err := os.WriteFile(probe, []byte{}, 0o600); err != nil {
		return fmt.Errorf("write probe in %s: %w", path, err)
	}
	_ = os.Remove(probe)
	return nil
}

// ensurePoolSubdirs creates the per-pool subdirectories the storage-
// image surface depends on: `images/` for the cached image files (the
// path Manager.Create clones from) and `scratch/import/` for in-progress
// downloads. Idempotent — MkdirAll returns nil on existing directories.
func ensurePoolSubdirs(root string) error {
	for _, sub := range []string{
		filepath.Join(root, imagesSubdir),
		filepath.Join(root, "scratch", "import"),
		filepath.Join(root, snapshotsSubdir),
		filepath.Join(root, snapshotsSubdir, snapshotsStagingSubdir),
		filepath.Join(root, snapshotsSubdir, snapshotManifestsSubdir),
	} {
		if err := os.MkdirAll(sub, 0o750); err != nil {
			return fmt.Errorf("mkdir %s: %w", sub, err)
		}
	}
	return nil
}

// Get returns a snapshot of the VM identified by id. Returns ErrNotFound
// when no such VM exists. The Status field reflects what's persisted on
// disk plus a fresh process-supervision probe so callers see live state.
func (m *Manager) Get(id uuid.UUID) (*VM, error) {
	m.mu.Lock()
	v, ok := m.vms[id]
	if !ok {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	cp := *v
	m.mu.Unlock()
	cp.Status = m.observedStatus(&cp)
	return &cp, nil
}

// ByName returns a snapshot of the VM identified by name. The
// agent's wire surface addresses VMs by name; this is the resolver
// the handlers hit on every name-keyed request. Returns ErrNotFound
// when no live (non-deleted) entry matches.
func (m *Manager) ByName(name string) (*VM, error) {
	m.mu.Lock()
	var found *VM
	for _, v := range m.vms {
		if v.Name == name {
			cp := *v
			found = &cp
			break
		}
	}
	m.mu.Unlock()
	if found == nil {
		return nil, ErrNotFound
	}
	found.Status = m.observedStatus(found)
	return found, nil
}

// List returns a snapshot of every VM, ordered by CreatedAt ascending.
func (m *Manager) List() []*VM {
	m.mu.Lock()
	out := make([]*VM, 0, len(m.vms))
	for _, v := range m.vms {
		cp := *v
		out = append(out, &cp)
	}
	m.mu.Unlock()
	for _, v := range out {
		v.Status = m.observedStatus(v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Task returns the agent task identified by id, or nil when missing.
func (m *Manager) Task(id uuid.UUID) *AgentTask { return m.tasks.Get(id) }

// observedStatus combines the persisted status with a quick liveness
// probe. For terminal statuses (failed, stopped) the persisted value
// wins; for running it gets revalidated against the pidfile.
func (m *Manager) observedStatus(v *VM) Status {
	switch v.Status {
	case StatusRunning:
		pid, err := qemu.ReadPIDFile(v.PIDFile)
		if err != nil || !qemu.IsAlive(pid) {
			return StatusStopped
		}
		return StatusRunning
	case StatusMigratingIncoming:
		// A dead paused -incoming target must not report eternal `migrating`:
		// probe the pidfile and downgrade to failed when the qemu is gone.
		// Belt-and-suspenders - recovery (reconcileRecoveredIncoming) runs once
		// at boot before the manager serves and normally transitions this VM
		// away from StatusMigratingIncoming first.
		pid, err := qemu.ReadPIDFile(v.PIDFile)
		if err != nil || !qemu.IsAlive(pid) {
			return StatusFailed
		}
		return StatusMigratingIncoming
	default:
		return v.Status
	}
}

// preResumeIncomingRunStates are the qemu query-status run-states that mean a
// recovered -incoming target never resumed: the guest CPUs are stopped while the
// migration is mid-flight or awaiting cont, so the qemu is a safe orphan to reap.
// A target killed mid-incoming reports "inmigrate" (still receiving the RAM
// stream) - the common real orphan; after the stream completes but before cont
// it reports "paused". The ONLY state meaning a live resumed guest (which must be
// promoted, never killed) is "running"; any successfully-queried state outside
// this set (e.g. "suspended") is treated as inconclusive and left alone (fail
// toward inaction).
var preResumeIncomingRunStates = map[string]bool{
	"inmigrate":      true,
	"paused":         true,
	"prelaunch":      true,
	"finish-migrate": true,
}

// reconcileRecoveredIncoming reconciles one orphaned StatusMigratingIncoming VM
// replayed at agent startup. It probes the adopted -incoming qemu and branches
// fail-closed:
//
//   - qemu alive AND running: a post-cutover guest whose StatusRunning write did
//     not survive the crash. PROMOTE - re-attach the mux and persist
//     StatusRunning. NEVER kill (it may be the only copy and it is live).
//   - qemu alive AND a pre-resume run-state (inmigrate / paused / prelaunch /
//     finish-migrate): a genuine orphaned incoming that never resumed. Reap the
//     LEAK only - kill the qemu (frees the RAM + NBD ingress ports its in-process
//     exports held) and tear down the host taps, then mark StatusFailed. LEAK the
//     destination disk for operator / CP recovery.
//   - qemu dead / dial fails / pidfile missing: orphaned, process already gone.
//     Tear down taps, mark StatusFailed. No qemu to kill. Leak the disk.
//   - INCONCLUSIVE (alive, queried, but a run-state outside the sets above, or the
//     query failed): do NOT kill. Mark StatusFailed without killing and log
//     loudly - killing requires a POSITIVE pre-resume signal or a confirmed-dead
//     process (fail toward inaction).
//
// It NEVER calls removeAdoptedVM: deleting the destination disk on recovery
// could destroy the only copy of a post-cutover guest. Runs inside New before
// the manager serves, so the VM reconciler cannot race it; m.vms is populated
// and the fabric is set, so transitionVM / persistVM / killIncoming /
// teardownNICs / attachMux are all safe to call here.
func (m *Manager) reconcileRecoveredIncoming(log *slog.Logger, v *VM) {
	probe := m.probeIncoming(v.QMPSocket, v.PIDFile)

	switch {
	case probe.alive && probe.queried && probe.runState == "running":
		// Promote: a resumed guest whose status write was lost.
		log.Info("recover: promoting resumed live-migration target to running",
			"vm", v.Name, "vm_id", v.ID.String())
		m.transitionVM(v.ID, StatusRunning, "")
		if err := m.attachMux(log, v); err != nil {
			log.Warn("recover: re-attach serial multiplexer for promoted target",
				"vm", v.Name, "err", err.Error())
		}
		if err := m.persistVM(v.ID); err != nil {
			log.Warn("recover: persist promoted target meta",
				"vm", v.Name, "err", err.Error())
		}

	case probe.alive && probe.queried && preResumeIncomingRunStates[probe.runState]:
		// Genuine orphaned incoming (never resumed): reap the leaked qemu, never
		// touch the disk.
		log.Warn("recover: reaping orphaned pre-resume live-migration target",
			"vm", v.Name, "vm_id", v.ID.String(), "run_state", probe.runState,
			"reason", "recovered orphaned incoming live migration")
		m.killIncoming(v)
		m.teardownNICs(v.NICs)
		m.transitionVM(v.ID, StatusFailed, "")
		if err := m.persistVM(v.ID); err != nil {
			log.Warn("recover: persist reaped target meta",
				"vm", v.Name, "err", err.Error())
		}

	case !probe.alive:
		// Process already gone: nothing to kill. Tear down taps, mark failed,
		// leak the disk for recovery.
		log.Warn("recover: orphaned live-migration target qemu already gone",
			"vm", v.Name, "vm_id", v.ID.String(),
			"reason", "recovered orphaned incoming live migration; qemu gone")
		m.teardownNICs(v.NICs)
		m.transitionVM(v.ID, StatusFailed, "")
		if err := m.persistVM(v.ID); err != nil {
			log.Warn("recover: persist gone target meta",
				"vm", v.Name, "err", err.Error())
		}

	default:
		// INCONCLUSIVE: alive but dial/query failed or an unexpected run-state.
		// Fail toward inaction - do NOT kill (no positive paused signal, process
		// not confirmed dead). Mark failed and log loudly so an operator
		// investigates the half-recovered qemu.
		log.Error("recover: inconclusive probe of orphaned live-migration target; NOT killing",
			"vm", v.Name, "vm_id", v.ID.String(),
			"alive", probe.alive, "queried", probe.queried, "run_state", probe.runState,
			"reason", "recovered orphaned incoming live migration; inconclusive probe")
		m.transitionVM(v.ID, StatusFailed, "")
		if err := m.persistVM(v.ID); err != nil {
			log.Warn("recover: persist inconclusive target meta",
				"vm", v.Name, "err", err.Error())
		}
	}
}

// Create begins async VM creation and returns the agent task tracking it.
// The HTTP layer responds 202 immediately with the task ID; subsequent
// GET /v1/tasks/{id} polls reveal progress.
func (m *Manager) Create(ctx context.Context, spec CreateSpec) (*AgentTask, error) {
	if err := validateCreateSpec(spec); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSpec, err)
	}
	m.poolsMu.RLock()
	p, ok := m.pools[spec.PoolName]
	m.poolsMu.RUnlock()
	if !ok {
		return nil, ErrPoolUnknown
	}

	vmID := spec.UUID
	if vmID == uuid.Nil {
		vmID = uuid.New()
	}

	arch := qemu.HostArch()

	disk, qmp, console, pid := m.vmPaths(p.root, vmID)
	v := &VM{
		ID:            vmID,
		Name:          spec.Name,
		VCPUs:         spec.VCPUs,
		MemoryMB:      spec.MemoryMB,
		PoolName:      spec.PoolName,
		Architecture:  arch,
		Status:        StatusPending,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
		DiskPath:      disk,
		QMPSocket:     qmp,
		ConsoleSocket: console,
		PIDFile:       pid,
		NICs:          spec.NICs,
	}
	if needsCidata(spec) {
		v.CidataPath = filepath.Join(m.stateDir, vmID.String(), "cidata.iso")
	}

	// Durable dedup: m.vms is reloaded from disk on restart, so a redelivered
	// vm.create must be deduped on the DURABLE m.vms[vmID], not only on the
	// in-memory createTasks map (which is rebuilt EMPTY on restart). A job
	// redelivered before the CP persisted the agent_task_id - after the agent
	// restarted - re-POSTs this create; overwriting m.vms[vmID] and spawning a
	// second runCreate would clobber the live VM's disk and re-materialise its
	// NICs (tap churn). Never overwrite an existing VM. The duplicate check and
	// the insert share one critical section so two concurrent first attempts
	// cannot both materialise the VM.
	m.mu.Lock()
	if _, exists := m.vms[vmID]; exists {
		origTaskID, hasTask := m.createTasks[vmID]
		var reloadedStatus Status
		if !hasTask {
			reloadedStatus = m.vms[vmID].Status
		}
		m.mu.Unlock()
		if hasTask {
			// In-process resumption: echo the ORIGINAL create task so the CP
			// resumes against the same agent_task_id, leaving the live VM and
			// its taps untouched.
			if existing := m.tasks.Get(origTaskID); existing != nil {
				return existing, nil
			}
			// The record exists but the task is gone (should not happen while
			// the process is up). Treat it as a conflict the handler maps to 409.
			return nil, ErrCreateInFlight
		}
		// Post-restart redelivery: no live create task. Mint an idempotent task
		// reflecting the reloaded VM status WITHOUT overwriting or re-spawning.
		return m.idempotentCreateResult(vmID, reloadedStatus), nil
	}
	task := m.tasks.Create(TaskKindVMCreate, vmID)
	m.vms[vmID] = v
	m.createTasks[vmID] = task.ID
	m.mu.Unlock()

	// #nosec G118 -- async task work intentionally outlives the HTTP request;
	// the task surface (GET /v1/tasks/{id}) is how clients track progress.
	go m.runCreate(task.ID, v, spec)
	return task, nil
}

// idempotentCreateResult mints a terminal create task that reflects an
// already-present VM's status, for a redelivered create that must NOT
// re-materialise the VM. A completed VM (running / stopped / paused) reports
// success so the CP reconciles; a failed VM reports failed; an in-progress VM
// with no live task was interrupted (crash mid-create) and reports failed
// (vm_create_interrupted) rather than re-spawn (which could clobber). The VM
// record itself is never mutated here. The success result carries vm_id only
// (the image digest / sizes from the original create are not re-derived); the
// CP's createResultFromTerminal degrades gracefully, dropping just the
// correlation metadata, so the VM still projects to success.
func (m *Manager) idempotentCreateResult(vmID uuid.UUID, status Status) *AgentTask {
	t := m.tasks.Create(TaskKindVMCreate, vmID)
	switch status {
	case StatusRunning, StatusStopped, StatusPaused:
		result, _ := json.Marshal(map[string]string{"vm_id": vmID.String()})
		m.tasks.Update(t.ID, func(at *AgentTask) {
			at.Status = TaskStatusSuccess
			at.Result = result
		})
	case StatusFailed:
		m.tasks.Update(t.ID, func(at *AgentTask) {
			at.Status = TaskStatusFailed
			at.Error = &TaskError{Code: "vm_create_failed", Message: "vm create previously failed"}
		})
	default: // creating / pending / stopping / deleting, no live task -> interrupted prior attempt.
		m.tasks.Update(t.ID, func(at *AgentTask) {
			at.Status = TaskStatusFailed
			at.Error = &TaskError{Code: "vm_create_interrupted", Message: "vm create was interrupted; recreate the vm"}
		})
	}
	return m.tasks.Get(t.ID)
}

// sizeCreatedDisk sizes the freshly-cloned per-VM disk at diskPath (never the
// shared cache file). qemu-img info reports the image virtual size; a positive
// diskGiB below it is rejected (shrink is not allowed) and a larger diskGiB
// grows the clone via qemu-img resize. It returns the image virtual size, the
// resulting disk size, and a (failCode, failMsg) pair - failCode is "" on
// success, otherwise it carries the task-failure code the caller surfaces.
func (m *Manager) sizeCreatedDisk(ctx context.Context, diskPath string, diskGiB int) (virtualSize, diskSize int64, failCode, failMsg string) {
	info, err := qemu.ImgInfoOf(ctx, diskPath)
	if err != nil {
		return 0, 0, "internal", fmt.Sprintf("qemu-img info: %v", err)
	}
	diskSize = info.VirtualSize
	if diskGiB > 0 {
		want := int64(diskGiB) * 1073741824
		if want < info.VirtualSize {
			return 0, 0, "disk_too_small",
				fmt.Sprintf("requested disk %d bytes is below image virtual size %d", want, info.VirtualSize)
		}
		if want > info.VirtualSize {
			if err := qemu.ResizeImg(ctx, diskPath, want); err != nil {
				return 0, 0, "internal", fmt.Sprintf("resize disk: %v", err)
			}
			diskSize = want
		}
	}
	return info.VirtualSize, diskSize, "", ""
}

// materialiseCreateDisk prepares the per-VM boot disk for a fresh create,
// branching on the disk source:
//
//   - Migrated VM: the disk was copied in from the source node and is already
//     authoritative - re-cloning would clobber the migrated state, so skip
//     everything (ensured stays zero, sizes 0).
//   - Recreate-from-snapshot (spec.SourceSnapshot != nil): clone each referenced
//     content-addressed blob from {poolRoot}/snapshots/{sha}.qcow2 in disk-index
//     order; no image download. The blob is the disk at its captured size, so no
//     qemu-img resize/info is performed (DiskGiB is not honored on recreate).
//   - Image source: EnsureImageInto (basename-keyed IfNotPresent cache) clones
//     the base image into the per-VM disk, then sizeCreatedDisk applies DiskGiB.
//
// It returns the resolved base-image ensure result (zero for the migrated and
// snapshot branches), the image virtual size and resulting disk size (0 on the
// non-image branches), and a (failCode, failMsg) pair - failCode is "" on
// success, otherwise the task-failure code the caller surfaces.
func (m *Manager) materialiseCreateDisk(ctx context.Context, v *VM, spec CreateSpec) (ensured EnsureResult, virtualSize, diskSize int64, failCode, failMsg string) {
	switch {
	case v.Migrated:
		return EnsureResult{}, 0, 0, "", ""
	case spec.SourceSnapshot != nil:
		if failCode, failMsg = m.materialiseFromSnapshot(v, spec.SourceSnapshot); failCode != "" {
			return EnsureResult{}, 0, 0, failCode, failMsg
		}
		return EnsureResult{}, 0, 0, "", ""
	default:
		// EnsureImageInto holds the per-image lock across the ensure and the clone
		// so a concurrent ensure cannot overwrite the cache file between the digest
		// verify and the clone (audit R2-L5).
		var err error
		ensured, err = m.EnsureImageInto(ctx, v.PoolName, spec.ImageURL, spec.ExpectedSHA256, spec.Format, v.DiskPath)
		if err != nil {
			var ce *ChecksumMismatchError
			switch {
			case errors.As(err, &ce):
				return EnsureResult{}, 0, 0, "checksum_mismatch", ce.Error()
			case errors.Is(err, ErrCloneFailed):
				return EnsureResult{}, 0, 0, "clone_failed", err.Error()
			default:
				return EnsureResult{}, 0, 0, "image_unavailable", err.Error()
			}
		}
		virtualSize, diskSize, failCode, failMsg = m.sizeCreatedDisk(ctx, v.DiskPath, spec.DiskGiB)
		return ensured, virtualSize, diskSize, failCode, failMsg
	}
}

// materialiseFromSnapshot clones every disk in ref from the pool's snapshots/
// subdir into the per-VM disk path, in virtio-index order. Each blob must exist
// (the CP resolved these digests from the snapshot manifest; a missing blob is a
// placement/replication gap surfaced as snapshot_blob_missing). Slice A VMs
// carry a single boot disk: index 0 clones into v.DiskPath (the boot disk the
// qemu command line attaches); any higher-index disk clones alongside it as
// disk{i}.qcow2 to preserve content even though the slice-A guest attaches only
// the boot disk. Returns a (failCode, failMsg) pair - failCode is "" on success.
func (m *Manager) materialiseFromSnapshot(v *VM, ref *SnapshotRef) (failCode, failMsg string) {
	poolRoot, err := m.poolRoot(v.PoolName)
	if err != nil {
		return "pool_not_found", fmt.Sprintf("resolve pool %q: %v", v.PoolName, err)
	}
	if len(ref.Disks) == 0 {
		return "snapshot_blob_missing", "snapshot has no disks to materialise"
	}
	snapshotsDir := filepath.Join(poolRoot, snapshotsSubdir)

	ordered := make([]SnapshotDiskRef, len(ref.Disks))
	copy(ordered, ref.Disks)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })

	for n, d := range ordered {
		blobPath := filepath.Join(snapshotsDir, d.SHA256+".qcow2")
		if _, err := os.Stat(blobPath); err != nil {
			return "snapshot_blob_missing",
				fmt.Sprintf("snapshot blob %s (disk %d) not present on pool %q: %v", d.SHA256, d.Index, v.PoolName, err)
		}
		dst := v.DiskPath
		if n > 0 {
			dst = filepath.Join(filepath.Dir(v.DiskPath), fmt.Sprintf("disk%d.qcow2", n))
		}
		if err := storage.CloneImage(blobPath, dst); err != nil {
			return "clone_failed", fmt.Sprintf("clone snapshot blob %s into %s: %v", d.SHA256, dst, err)
		}
	}
	return "", ""
}

func (m *Manager) runCreate(taskID uuid.UUID, v *VM, spec CreateSpec) {
	// The create work outlives the HTTP request that spawned it (the task
	// surface is how clients track progress), so it runs on a fresh
	// background context rather than the cancelled request context. The
	// image download enforces its own timeout (importHTTPTimeout).
	ctx := context.Background()
	log := m.log.With("vm_id", v.ID.String(), "task_id", taskID.String())
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	m.transitionVM(v.ID, StatusCreating, "")
	if err := m.persistVM(v.ID); err != nil {
		log.Error("persist meta.json (creating)", "err", err)
		m.failTask(taskID, v.ID, "internal", err.Error())
		return
	}

	if err := os.MkdirAll(filepath.Join(m.stateDir, v.ID.String()), 0o750); err != nil {
		log.Error("mkdir state vm dir", "err", err)
		m.failTask(taskID, v.ID, "internal", err.Error())
		return
	}

	ensured, virtualSize, diskSize, failCode, failMsg := m.materialiseCreateDisk(ctx, v, spec)
	if failCode != "" {
		log.Error("materialise disk", "code", failCode, "err", failMsg)
		m.failTask(taskID, v.ID, failCode, failMsg)
		return
	}

	if v.CidataPath != "" {
		if err := m.createBuildCidata(v.CidataPath, v.Name, spec.UserData, spec.NetworkData); err != nil {
			log.Error("build cidata iso", "err", err)
			m.failTask(taskID, v.ID, "cloudinit_failed", err.Error())
			return
		}
	}

	// Materialise host taps before qemu launches so the tap netdevs the
	// command line references already exist. Bridge creation is the
	// network reconciler's job - the bridge is assumed present here.
	if err := m.materialiseNICs(v.NICs); err != nil {
		log.Error("materialise nics", "err", err)
		m.failTask(taskID, v.ID, "network_setup_failed", err.Error())
		return
	}

	if code, err := m.spawnAndVerify(log, v); err != nil {
		m.teardownNICs(v.NICs)
		m.failTask(taskID, v.ID, code, err.Error())
		return
	}

	// Per ADR 0029 L13 the multiplexer is a required prerequisite -
	// if it cannot attach to the QEMU serial socket we tear the VM
	// down rather than leave operators with a half-broken console.
	// Same pattern runStart / runReboot apply for their spawn paths.
	if err := m.attachMux(log, v); err != nil {
		log.Error("attach multiplexer", "err", err)
		m.killQEMU(v)
		m.teardownNICs(v.NICs)
		m.failTask(taskID, v.ID, "multiplexer_failed", err.Error())
		return
	}

	m.transitionVM(v.ID, StatusRunning, "")
	if err := m.persistVM(v.ID); err != nil {
		log.Warn("persist meta.json (running)", "err", err)
	}
	result, _ := json.Marshal(map[string]any{
		"vm_id":              v.ID.String(),
		"image_sha256":       ensured.SHA256,
		"virtual_size_bytes": virtualSize,
		"disk_size_bytes":    diskSize,
	})
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusSuccess
		t.Result = result
	})
}

// spawnAndVerify boots a qemu process for v, verifies the pidfile is
// alive, and confirms QMP responds to query-status. Returns ("", nil) on
// success or (code, err) where `code` is the agent task error code
// the caller should pass to failTask. Pure side-effect: forks a
// daemonized qemu process; does not mutate Manager state. Shared by
// runCreate (fresh VM, first boot) and runStart (existing VM, restart
// after stop / poweroff).
func (m *Manager) spawnAndVerify(log *slog.Logger, v *VM) (string, error) {
	binary, err := qemu.Binary(v.Architecture)
	if err != nil {
		log.Error("qemu binary", "err", err)
		return "internal", err
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
		CidataPath:      v.CidataPath,
		NICs:            v.NICs,
	})
	if err != nil {
		log.Error("build qemu args", "err", err)
		return "internal", err
	}

	spawnCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := qemu.Spawn(spawnCtx, binary, args); err != nil {
		log.Error("spawn qemu", "err", err)
		return "qemu_spawn_failed", err
	}

	pid, err := qemu.ReadPIDFile(v.PIDFile)
	if err != nil {
		log.Error("read pidfile", "err", err)
		return "qemu_supervision_failed", err
	}
	if !qemu.IsAlive(pid) {
		log.Error("qemu exited immediately after daemonize", "pid", pid)
		return "qemu_supervision_failed", fmt.Errorf("pid %d is not alive", pid)
	}

	client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
	if err != nil {
		log.Error("qmp dial", "err", err)
		return "qmp_unavailable", err
	}
	status, err := client.QueryStatus()
	_ = client.Close()
	if err != nil {
		log.Error("qmp query-status", "err", err)
		return "qmp_unavailable", err
	}
	log.Info("vm running", "qmp_status", status, "pid", pid)
	return "", nil
}

// Pause issues QMP `stop` against the running guest and transitions
// the persisted status to paused. Returns ErrNotFound when no VM
// matches; ErrInvalidState when the VM is not currently observed
// as running; ErrQMPUnavailable when the QMP dial or command call
// fails. Synchronous — the QMP `stop` command returns immediately.
//
// Per Area 4-IV the agent does not auto-retry on QMP failure: the
// VM stays in its prior phase, the error surfaces to the operator,
// and they decide whether to re-invoke.
func (m *Manager) Pause(ctx context.Context, name string) (*VM, error) {
	return m.runSyncLifecycle(ctx, name, "pause", func(v *VM, observed Status) (Status, error) {
		if observed != StatusRunning {
			return "", fmt.Errorf("%w: pause requires phase=running, got %q", ErrInvalidState, observed)
		}
		client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
		if err != nil {
			return "", fmt.Errorf("%w: dial: %v", ErrQMPUnavailable, err)
		}
		defer func() { _ = client.Close() }()
		if err := client.Stop(); err != nil {
			return "", fmt.Errorf("%w: stop: %v", ErrQMPUnavailable, err)
		}
		return StatusPaused, nil
	})
}

// Resume issues QMP `cont` against the paused guest and transitions
// the persisted status back to running. Returns ErrInvalidState
// when the observed status is not paused.
func (m *Manager) Resume(ctx context.Context, name string) (*VM, error) {
	return m.runSyncLifecycle(ctx, name, "resume", func(v *VM, observed Status) (Status, error) {
		if observed != StatusPaused {
			return "", fmt.Errorf("%w: resume requires phase=paused, got %q", ErrInvalidState, observed)
		}
		client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
		if err != nil {
			return "", fmt.Errorf("%w: dial: %v", ErrQMPUnavailable, err)
		}
		defer func() { _ = client.Close() }()
		if err := client.Cont(); err != nil {
			return "", fmt.Errorf("%w: cont: %v", ErrQMPUnavailable, err)
		}
		return StatusRunning, nil
	})
}

// Reset issues QMP `system_reset` against the running guest. The
// QEMU process keeps running and the guest CPU is reset; the
// persisted status stays running (the wire `phase` is unchanged
// because runtime-side identity is preserved — operators detect
// the reboot through guest uptime, not through CP-side state).
// Returns ErrInvalidState when the observed status is not running.
func (m *Manager) Reset(ctx context.Context, name string) (*VM, error) {
	return m.runSyncLifecycle(ctx, name, "reset", func(v *VM, observed Status) (Status, error) {
		if observed != StatusRunning {
			return "", fmt.Errorf("%w: reset requires phase=running, got %q", ErrInvalidState, observed)
		}
		client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
		if err != nil {
			return "", fmt.Errorf("%w: dial: %v", ErrQMPUnavailable, err)
		}
		defer func() { _ = client.Close() }()
		if err := client.SystemReset(); err != nil {
			return "", fmt.Errorf("%w: system_reset: %v", ErrQMPUnavailable, err)
		}
		return StatusRunning, nil
	})
}

// runSyncLifecycle is the shared engine for Pause / Resume / Reset.
// It resolves the VM by name, snapshots the entry under the manager
// mutex, runs the supplied action without holding the lock (so QMP
// dial does not block other handlers), and applies the resulting
// status transition + meta.json persist under the mutex. Returns the
// post-transition VM snapshot for the handler's 200 response.
//
// Failures are logged at the agent's component logger so operators
// can correlate the structured handler 5xx with the underlying QMP
// fault; the persisted phase is unchanged on failure (Area 4-IV).
func (m *Manager) runSyncLifecycle(
	_ context.Context,
	name, op string,
	action func(v *VM, observed Status) (Status, error),
) (*VM, error) {
	v, err := m.ByName(name)
	if err != nil {
		return nil, err
	}
	observed := v.Status

	newStatus, err := action(v, observed)
	if err != nil {
		m.log.Error("vm sync lifecycle action failed",
			"op", op, "vm_name", name, "vm_id", v.ID.String(), "observed_status", string(observed), "err", err)
		return nil, err
	}

	m.transitionVM(v.ID, newStatus, "")
	if persistErr := m.persistVM(v.ID); persistErr != nil {
		m.log.Warn("vm sync lifecycle persist meta failed",
			"op", op, "vm_name", name, "vm_id", v.ID.String(), "err", persistErr)
	}

	updated, err := m.ByName(name)
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// Start begins async VM boot and returns the agent task tracking it.
// Validates that the VM is in a startable observed phase (stopped,
// failed, or already running — a start-when-running is a task-success
// no-op per CP spec). Spawns the QEMU process, verifies QMP responds,
// then transitions persisted status to running. Returns ErrNotFound
// when no VM matches; ErrInvalidState when phase is paused / creating
// / deleting (use resume or wait, not start).
func (m *Manager) Start(ctx context.Context, name string) (*AgentTask, error) {
	v, err := m.ByName(name)
	if err != nil {
		return nil, err
	}
	switch v.Status {
	case StatusStopped, StatusFailed, StatusRunning:
	default:
		return nil, fmt.Errorf("%w: start requires phase ∈ {stopped, failed, running}, got %q", ErrInvalidState, v.Status)
	}
	release, ok := m.inFlightAcquire(v.Name)
	if !ok {
		return nil, ErrInFlight
	}
	task := m.tasks.Create(TaskKindVMStart, v.ID)
	// #nosec G118 -- async task work intentionally outlives the HTTP request.
	go func() {
		defer release()
		m.runStart(task.ID, v.ID, v.Status)
	}()
	return task, nil
}

func (m *Manager) runStart(taskID, vmID uuid.UUID, observed Status) {
	log := m.log.With("vm_id", vmID.String(), "task_id", taskID.String(), "op", "start")
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	if observed == StatusRunning {
		log.Info("vm already running; start is a no-op")
		result, _ := json.Marshal(map[string]string{"vm_id": vmID.String()})
		m.tasks.Update(taskID, func(t *AgentTask) {
			t.Status = TaskStatusSuccess
			t.Result = result
		})
		return
	}

	v, err := m.snapshotVM(vmID)
	if err != nil {
		log.Error("snapshot vm", "err", err)
		m.failTask(taskID, vmID, "internal", err.Error())
		return
	}

	// A post-cutover start of a just-migrated VM must release the incoming
	// migration's qemu-nbd before qemu opens the disk: that server holds an
	// exclusive write lock and would make the spawn fail with "Failed to get
	// write lock". Deterministic regardless of how many connections the
	// transfer used; a no-op for a normal (non-migration) start.
	m.releaseIncomingNBD(vmID)

	if code, err := m.spawnAndVerify(log, v); err != nil {
		m.failTask(taskID, vmID, code, err.Error())
		return
	}

	// Per ADR 0029 L13 the multiplexer is a required prerequisite -
	// if it cannot attach to the QEMU serial socket we tear the VM
	// down rather than leave operators with a half-broken console.
	if err := m.attachMux(log, v); err != nil {
		log.Error("attach multiplexer", "err", err)
		m.killQEMU(v)
		m.failTask(taskID, vmID, "multiplexer_failed", err.Error())
		return
	}

	m.transitionVM(vmID, StatusRunning, "")
	if err := m.persistVM(vmID); err != nil {
		log.Warn("persist meta.json (running)", "err", err)
	}
	result, _ := json.Marshal(map[string]string{"vm_id": vmID.String()})
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusSuccess
		t.Result = result
	})
}

// Stop begins async graceful VM shutdown via ACPI (QMP
// system_powerdown). The agent waits up to shutdownGrace (60s) for the
// guest to honour the signal; if the guest does not exit within the
// window the task completes as failed with code `stop_timeout` and the
// observed phase stays running per Area 4-II lock (operators dispatch
// to poweroff via the CLI `--force` flag or the explicit poweroff
// endpoint).
func (m *Manager) Stop(ctx context.Context, name string) (*AgentTask, error) {
	v, err := m.ByName(name)
	if err != nil {
		return nil, err
	}
	if v.Status != StatusRunning {
		return nil, fmt.Errorf("%w: stop requires phase=running, got %q", ErrInvalidState, v.Status)
	}
	release, ok := m.inFlightAcquire(v.Name)
	if !ok {
		return nil, ErrInFlight
	}
	task := m.tasks.Create(TaskKindVMStop, v.ID)
	// #nosec G118 -- async task work intentionally outlives the HTTP request.
	go func() {
		defer release()
		m.runStop(task.ID, v.ID)
	}()
	return task, nil
}

func (m *Manager) runStop(taskID, vmID uuid.UUID) {
	log := m.log.With("vm_id", vmID.String(), "task_id", taskID.String(), "op", "stop")
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	v, err := m.snapshotVM(vmID)
	if err != nil {
		log.Error("snapshot vm", "err", err)
		m.failTask(taskID, vmID, "internal", err.Error())
		return
	}
	m.transitionVM(vmID, StatusStopping, "")
	if persistErr := m.persistVM(vmID); persistErr != nil {
		log.Warn("persist meta.json (stopping)", "err", persistErr)
	}

	pid, err := qemu.ReadPIDFile(v.PIDFile)
	if err != nil {
		log.Error("read pidfile", "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "qemu_supervision_failed", err.Error())
		return
	}

	client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
	if err != nil {
		log.Error("qmp dial", "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "qmp_unavailable", err.Error())
		return
	}
	if err := client.SystemPowerdown(); err != nil {
		_ = client.Close()
		log.Error("qmp system_powerdown", "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "qmp_unavailable", err.Error())
		return
	}
	_ = client.Close()

	waitCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := qemu.WaitGone(waitCtx, pid, shutdownGrace); err != nil {
		log.Warn("graceful shutdown timed out", "pid", pid, "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "stop_timeout",
			fmt.Sprintf("guest did not honour ACPI shutdown within %s; use poweroff or stop --force", shutdownGrace))
		return
	}

	m.transitionVM(vmID, StatusStopped, "")
	if err := m.persistVM(vmID); err != nil {
		log.Warn("persist meta.json (stopped)", "err", err)
	}
	result, _ := json.Marshal(map[string]string{"vm_id": vmID.String()})
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusSuccess
		t.Result = result
	})
	log.Info("vm stopped (acpi)")
}

// Poweroff begins async hard shutdown. Attempts QMP `quit` first (so
// qemu releases sockets / pidfile cleanly) and falls back to SIGKILL
// after poweroffGrace (5s) if quit does not land. The guest OS is not
// notified per spec; data loss inside the guest is a documented
// possibility. Returns ErrInvalidState only when phase is pending or
// creating (the VM is mid-spawn — operators wait); every other phase
// admits poweroff (the wire intent is "make it stop").
func (m *Manager) Poweroff(ctx context.Context, name string) (*AgentTask, error) {
	v, err := m.ByName(name)
	if err != nil {
		return nil, err
	}
	switch v.Status {
	case StatusPending, StatusCreating, StatusDeleting:
		return nil, fmt.Errorf("%w: poweroff requires a past-spawn phase, got %q", ErrInvalidState, v.Status)
	}
	release, ok := m.inFlightAcquire(v.Name)
	if !ok {
		return nil, ErrInFlight
	}
	task := m.tasks.Create(TaskKindVMPoweroff, v.ID)
	// #nosec G118 -- async task work intentionally outlives the HTTP request.
	go func() {
		defer release()
		m.runPoweroff(task.ID, v.ID)
	}()
	return task, nil
}

func (m *Manager) runPoweroff(taskID, vmID uuid.UUID) {
	log := m.log.With("vm_id", vmID.String(), "task_id", taskID.String(), "op", "poweroff")
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	v, err := m.snapshotVM(vmID)
	if err != nil {
		log.Error("snapshot vm", "err", err)
		m.failTask(taskID, vmID, "internal", err.Error())
		return
	}
	m.transitionVM(vmID, StatusStopping, "")
	if persistErr := m.persistVM(vmID); persistErr != nil {
		log.Warn("persist meta.json (stopping)", "err", persistErr)
	}

	pid, err := qemu.ReadPIDFile(v.PIDFile)
	if err != nil || pid <= 0 || !qemu.IsAlive(pid) {
		log.Info("qemu already gone; poweroff is a no-op", "pid", pid, "err", err)
		m.transitionVM(vmID, StatusStopped, "")
		_ = m.persistVM(vmID)
		result, _ := json.Marshal(map[string]string{"vm_id": vmID.String()})
		m.tasks.Update(taskID, func(t *AgentTask) {
			t.Status = TaskStatusSuccess
			t.Result = result
		})
		return
	}

	if client, dialErr := qemu.DialQMP(v.QMPSocket, 5*time.Second); dialErr == nil {
		if err := client.Quit(); err != nil {
			log.Warn("qmp quit failed; falling through to SIGKILL", "err", err)
		}
		_ = client.Close()
	} else {
		log.Warn("qmp dial during poweroff failed; falling through to SIGKILL", "err", dialErr)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), poweroffGrace)
	if err := qemu.WaitGone(waitCtx, pid, poweroffGrace); err != nil {
		cancel()
		log.Warn("qmp quit did not land within poweroffGrace; sending SIGKILL", "pid", pid)
		if killErr := qemu.Kill(pid); killErr != nil {
			log.Error("SIGKILL failed", "pid", pid, "err", killErr)
			m.failTask(taskID, vmID, "qemu_supervision_failed", killErr.Error())
			return
		}
		killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := qemu.WaitGone(killCtx, pid, 5*time.Second); err != nil {
			killCancel()
			log.Error("SIGKILL did not land", "pid", pid, "err", err)
			m.failTask(taskID, vmID, "qemu_supervision_failed", err.Error())
			return
		}
		killCancel()
	} else {
		cancel()
	}

	m.transitionVM(vmID, StatusStopped, "")
	if err := m.persistVM(vmID); err != nil {
		log.Warn("persist meta.json (stopped)", "err", err)
	}
	result, _ := json.Marshal(map[string]string{"vm_id": vmID.String()})
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusSuccess
		t.Result = result
	})
	log.Info("vm powered off")
}

// Reboot begins async graceful reboot — orchestrates an internal stop
// followed by an internal start (Area 4-III lock — reboot ≠ reset; the
// QEMU process is replaced so the PID changes). Stop honours
// shutdownGrace; if the guest does not exit within the window the
// reboot task fails with code `stop_timeout` and the observed phase stays
// running (operators dispatch to reset via the dedicated endpoint).
func (m *Manager) Reboot(ctx context.Context, name string) (*AgentTask, error) {
	v, err := m.ByName(name)
	if err != nil {
		return nil, err
	}
	if v.Status != StatusRunning {
		return nil, fmt.Errorf("%w: reboot requires phase=running, got %q", ErrInvalidState, v.Status)
	}
	release, ok := m.inFlightAcquire(v.Name)
	if !ok {
		return nil, ErrInFlight
	}
	task := m.tasks.Create(TaskKindVMReboot, v.ID)
	// #nosec G118 -- async task work intentionally outlives the HTTP request.
	go func() {
		defer release()
		m.runReboot(task.ID, v.ID)
	}()
	return task, nil
}

func (m *Manager) runReboot(taskID, vmID uuid.UUID) {
	log := m.log.With("vm_id", vmID.String(), "task_id", taskID.String(), "op", "reboot")
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	v, err := m.snapshotVM(vmID)
	if err != nil {
		log.Error("snapshot vm", "err", err)
		m.failTask(taskID, vmID, "internal", err.Error())
		return
	}
	m.transitionVM(vmID, StatusStopping, "")
	if persistErr := m.persistVM(vmID); persistErr != nil {
		log.Warn("persist meta.json (stopping)", "err", persistErr)
	}

	pid, err := qemu.ReadPIDFile(v.PIDFile)
	if err != nil {
		log.Error("read pidfile", "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "qemu_supervision_failed", err.Error())
		return
	}

	client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
	if err != nil {
		log.Error("qmp dial", "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "qmp_unavailable", err.Error())
		return
	}
	if err := client.SystemPowerdown(); err != nil {
		_ = client.Close()
		log.Error("qmp system_powerdown", "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "qmp_unavailable", err.Error())
		return
	}
	_ = client.Close()

	waitCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	if err := qemu.WaitGone(waitCtx, pid, shutdownGrace); err != nil {
		cancel()
		log.Warn("reboot stop phase timed out", "pid", pid, "err", err)
		m.transitionVM(vmID, StatusRunning, "")
		_ = m.persistVM(vmID)
		m.failTaskOnly(taskID, "stop_timeout",
			fmt.Sprintf("guest did not honour ACPI shutdown within %s; use reset to force", shutdownGrace))
		return
	}
	cancel()

	m.transitionVM(vmID, StatusStopped, "")
	if err := m.persistVM(vmID); err != nil {
		log.Warn("persist meta.json (intermediate stopped)", "err", err)
	}

	v2, err := m.snapshotVM(vmID)
	if err != nil {
		log.Error("snapshot vm post-stop", "err", err)
		m.failTask(taskID, vmID, "internal", err.Error())
		return
	}
	if code, err := m.spawnAndVerify(log, v2); err != nil {
		m.failTask(taskID, vmID, code, err.Error())
		return
	}

	// Per ADR 0029 L15 the multiplexer is reused across reboot so
	// subscribers and the log file stay continuous. If the existing
	// entry's Reconnect fails (e.g. the QEMU socket path moved), fall
	// back to a fresh attachMux - existing subscribers see Done() on
	// the dead instance and the next reconnect from clients lands on
	// the replacement.
	if err := m.reconnectMux(log, v2); err != nil {
		log.Warn("reconnect multiplexer", "err", err)
	}

	m.transitionVM(vmID, StatusRunning, "")
	if err := m.persistVM(vmID); err != nil {
		log.Warn("persist meta.json (running)", "err", err)
	}
	result, _ := json.Marshal(map[string]string{"vm_id": vmID.String()})
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusSuccess
		t.Result = result
	})
	log.Info("vm rebooted")
}

// DeleteByName begins async VM teardown addressed by name, used by
// the Path D-rekeyed `DELETE /v1/vms/{vm_name}` agent handler.
// Resolves name → in-memory VM under the manager mutex, then delegates
// to Delete. ErrNotFound when no entry matches.
func (m *Manager) DeleteByName(ctx context.Context, name string) (*AgentTask, error) {
	m.mu.Lock()
	var id uuid.UUID
	for _, v := range m.vms {
		if v.Name == name {
			id = v.ID
			break
		}
	}
	m.mu.Unlock()
	if id == uuid.Nil {
		return nil, ErrNotFound
	}
	return m.Delete(ctx, id)
}

// Delete begins async VM teardown and returns the agent task tracking
// it. ErrNotFound when the VM is unknown.
func (m *Manager) Delete(ctx context.Context, vmID uuid.UUID) (*AgentTask, error) {
	m.mu.Lock()
	v, ok := m.vms[vmID]
	if !ok {
		m.mu.Unlock()
		return nil, ErrNotFound
	}
	name := v.Name
	v.Status = StatusDeleting
	v.UpdatedAt = time.Now().UTC()
	m.mu.Unlock()

	release, ok := m.inFlightAcquire(name)
	if !ok {
		return nil, ErrInFlight
	}
	task := m.tasks.Create(TaskKindVMDelete, vmID)
	// #nosec G118 -- async task work intentionally outlives the HTTP request.
	go func() {
		defer release()
		m.runDelete(task.ID, vmID)
	}()
	return task, nil
}

func (m *Manager) runDelete(taskID, vmID uuid.UUID) {
	log := m.log.With("vm_id", vmID.String(), "task_id", taskID.String())
	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusRunning })

	if err := m.persistVM(vmID); err != nil {
		log.Warn("persist meta.json (deleting)", "err", err)
	}

	v, err := m.snapshotVM(vmID)
	if err != nil {
		log.Error("snapshot vm", "err", err)
		m.failTask(taskID, vmID, "internal", err.Error())
		return
	}

	pid, _ := qemu.ReadPIDFile(v.PIDFile) // ignore error — VM may already be gone
	if pid > 0 && qemu.IsAlive(pid) {
		client, err := qemu.DialQMP(v.QMPSocket, 5*time.Second)
		if err == nil {
			if err := client.SystemPowerdown(); err != nil {
				log.Warn("system_powerdown failed; will fall back to force", "err", err)
			}
			_ = client.Close()
		} else {
			log.Warn("qmp dial during delete; falling back to kill", "err", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		if err := qemu.WaitGone(ctx, pid, shutdownGrace); err != nil {
			cancel()
			log.Warn("graceful shutdown timed out, sending SIGKILL", "pid", pid, "err", err)
			if killErr := qemu.Kill(pid); killErr != nil {
				log.Error("SIGKILL failed", "pid", pid, "err", killErr)
			}
			killCtx, killCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = qemu.WaitGone(killCtx, pid, 5*time.Second)
			killCancel()
		} else {
			cancel()
		}
	}

	// Tear down the multiplexer before removing the state directory so
	// its log file handle is closed first (ADR 0029 L16). detachMux is
	// keyed on VM name; the entry may already be absent (Stop /
	// Poweroff do not remove it but agent restart would).
	m.detachMux(v.Name)

	// Tear down host taps for each NIC. Best-effort — a failed DeleteTap
	// is logged and does not abort the delete (the in-memory VM and its
	// state directory still get removed).
	m.teardownNICs(v.NICs)

	// Cleanup disk + per-VM dirs. Errors logged but not fatal — operator
	// can clean stale files manually if needed.
	if err := os.RemoveAll(filepath.Dir(v.DiskPath)); err != nil {
		log.Warn("remove disk dir", "path", filepath.Dir(v.DiskPath), "err", err)
	}
	if err := os.RemoveAll(filepath.Join(m.stateDir, vmID.String())); err != nil {
		log.Warn("remove agent state dir", "err", err)
	}

	m.mu.Lock()
	delete(m.vms, vmID)
	m.mu.Unlock()

	m.tasks.Update(taskID, func(t *AgentTask) { t.Status = TaskStatusSuccess })
	log.Info("vm deleted")
}

// GetMux returns the registered serial multiplexer for the named VM,
// or nil when no multiplexer is currently registered (the VM is
// stopped/deleted, or the agent restarted while the VM was running
// and Start/Reboot have not yet re-attached one). Console and logs
// handlers call this to attach subscribers.
func (m *Manager) GetMux(name string) *serialmux.Multiplexer {
	m.muxesMu.Lock()
	defer m.muxesMu.Unlock()
	return m.muxes[name]
}

// attachMux opens a serial multiplexer for v and registers it under
// v.Name. A pre-existing entry (e.g. left behind by a failed Start)
// is Close'd before the new one is installed.
//
// Per L13 a multiplexer dial failure is fatal: callers tear the
// freshly-spawned QEMU down (via killQEMU) and fail the lifecycle
// task. Per L16 the per-VM state directory is the multiplexer's log
// directory; runDelete removes it after detachMux closes the file.
func (m *Manager) attachMux(log *slog.Logger, v *VM) error {
	logDir := filepath.Join(m.stateDir, v.ID.String())
	mux, err := serialmux.New(v.Name, v.ConsoleSocket, logDir, log)
	if err != nil {
		return err
	}
	m.muxesMu.Lock()
	if prior := m.muxes[v.Name]; prior != nil {
		_ = prior.Close()
	}
	m.muxes[v.Name] = mux
	m.muxesMu.Unlock()
	return nil
}

// reconnectMux is the Reboot helper: it asks the existing multiplexer
// to dial the new QEMU process. Subscribers and the log file stay
// continuous (L15). If the existing entry is missing or its Reconnect
// errors out, the multiplexer is replaced via attachMux - clients of
// the failed instance see Done() and reconnect to the fresh one.
func (m *Manager) reconnectMux(log *slog.Logger, v *VM) error {
	m.muxesMu.Lock()
	prior := m.muxes[v.Name]
	m.muxesMu.Unlock()
	if prior == nil {
		return m.attachMux(log, v)
	}
	if err := prior.Reconnect(); err != nil {
		_ = prior.Close()
		m.muxesMu.Lock()
		delete(m.muxes, v.Name)
		m.muxesMu.Unlock()
		if attachErr := m.attachMux(log, v); attachErr != nil {
			return fmt.Errorf("multiplexer reconnect failed (%v); fresh attach also failed: %v", err, attachErr)
		}
	}
	return nil
}

// detachMux closes the multiplexer registered under name (if any) and
// drops the registry entry. Idempotent.
func (m *Manager) detachMux(name string) {
	m.muxesMu.Lock()
	mux := m.muxes[name]
	delete(m.muxes, name)
	m.muxesMu.Unlock()
	if mux != nil {
		_ = mux.Close()
	}
}

// killQEMU best-effort terminates the QEMU process supervising v.
// Used by runStart when the multiplexer fails to attach: we cannot
// safely leave a running guest the agent has no observable channel
// into. Quiet on missing pidfile / already-dead process.
func (m *Manager) killQEMU(v *VM) {
	pid, err := qemu.ReadPIDFile(v.PIDFile)
	if err != nil || pid <= 0 {
		return
	}
	if !qemu.IsAlive(pid) {
		return
	}
	if err := qemu.Kill(pid); err != nil {
		m.log.Warn("killQEMU: SIGKILL failed", "pid", pid, "err", err)
		return
	}
	killCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = qemu.WaitGone(killCtx, pid, 3*time.Second)
}

func (m *Manager) snapshotVM(id uuid.UUID) (*VM, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *v
	return &cp, nil
}

// vmStatus returns the in-memory Status of the VM with id under the manager
// lock, and whether such a VM is tracked. Unlike Get it does NOT run an
// observedStatus pidfile probe: callers that need the raw recorded status (e.g.
// the cancel-reap guard distinguishing a paused -incoming target from a cont'd
// running guest) must not have that signal clouded by process supervision.
func (m *Manager) vmStatus(id uuid.UUID) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	if !ok {
		return "", false
	}
	return v.Status, true
}

func (m *Manager) transitionVM(id uuid.UUID, status Status, _ string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vms[id]
	if !ok {
		return
	}
	v.Status = status
	v.UpdatedAt = time.Now().UTC()
}

// vmPaths returns the on-disk paths for a VM with id in pool root poolRoot:
// the per-VM disk under the pool, and the qmp / console / pid sockets under
// the agent state dir. Create and AdoptForMigration share it so a
// destination-side adopt can never derive a path the create flow would not.
func (m *Manager) vmPaths(poolRoot string, id uuid.UUID) (disk, qmp, console, pid string) {
	disk = filepath.Join(poolRoot, "vms", id.String(), "disk.qcow2")
	qmp = filepath.Join(m.stateDir, id.String(), "qmp.sock")
	console = filepath.Join(m.stateDir, id.String(), "console.sock")
	pid = filepath.Join(m.stateDir, id.String(), "qemu.pid")
	return
}

func (m *Manager) persistVM(id uuid.UUID) error {
	v, err := m.snapshotVM(id)
	if err != nil {
		return err
	}
	meta := &state.VMMeta{
		VMID:          v.ID,
		Name:          v.Name,
		VCPUs:         v.VCPUs,
		MemoryMB:      v.MemoryMB,
		PoolName:      v.PoolName,
		Architecture:  string(v.Architecture),
		DiskPath:      v.DiskPath,
		QMPSocket:     v.QMPSocket,
		ConsoleSocket: v.ConsoleSocket,
		PIDFile:       v.PIDFile,
		CidataPath:    v.CidataPath,
		Status:        string(v.Status),
		CreatedAt:     v.CreatedAt,
		UpdatedAt:     v.UpdatedAt,
		NICs:          nicsToMeta(v.NICs),
		Migrated:      v.Migrated,
	}
	return state.WriteMeta(filepath.Join(m.stateDir, v.ID.String()), meta)
}

func (m *Manager) failTask(taskID, vmID uuid.UUID, code, message string) {
	m.transitionVM(vmID, StatusFailed, message)
	_ = m.persistVM(vmID)
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusFailed
		t.Error = &TaskError{Code: code, Message: message}
	})
}

// failTaskOnly marks the agent task as failed without mutating VM
// status. Reserved for stop / reboot failure paths where the QEMU
// process is still alive (ACPI ignored, QMP socket flapped, etc.);
// the orchestration failed but the VM is still running, so callers
// transition the VM back to StatusRunning + persist before calling
// this. Using failTask there would override the revert and persist
// a misleading StatusFailed, masking the live qemu process from the
// operator's recovery path (Start would re-spawn against a live
// pidfile lock; observedStatus on persisted Failed does not re-probe
// the pid).
func (m *Manager) failTaskOnly(taskID uuid.UUID, code, message string) {
	m.tasks.Update(taskID, func(t *AgentTask) {
		t.Status = TaskStatusFailed
		t.Error = &TaskError{Code: code, Message: message}
	})
}

// maxAgentDiskGiB mirrors the control-plane maxDiskGiB; the agent bounds
// disk_gib itself so a direct agent call or a buggy control plane cannot request
// a multi-PiB qemu-img resize (audit R2-L6).
const maxAgentDiskGiB = 65536

func validateCreateSpec(s CreateSpec) error {
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(s.Name) > 255 {
		return fmt.Errorf("name must be ≤ 255 chars")
	}
	if !validVMName(s.Name) {
		return fmt.Errorf("name must be a lowercase RFC1123 DNS label")
	}
	if s.VCPUs < 1 || s.VCPUs > 128 {
		return fmt.Errorf("vcpus must be in [1, 128]")
	}
	if s.MemoryMB < 128 || s.MemoryMB > 524288 {
		return fmt.Errorf("memory_mb must be in [128, 524288]")
	}
	if s.PoolName == "" {
		return fmt.Errorf("pool is required")
	}
	if err := validateDiskSource(s); err != nil {
		return err
	}
	if s.ExpectedSHA256 != "" && len(s.ExpectedSHA256) != 64 {
		return fmt.Errorf("expected_sha256 must be a 64-char sha256 hex digest")
	}
	if s.DiskGiB < 0 || s.DiskGiB > maxAgentDiskGiB {
		return fmt.Errorf("disk_gib must be in [0, %d]", maxAgentDiskGiB)
	}
	return nil
}

// validateDiskSource enforces exactly one disk source on a create spec: an image
// URL to download, or a recreate-from-snapshot ref whose content-addressed blobs
// the agent clones. The CP also enforces this; reaching the agent with neither
// or both is defense in depth - but the agent MUST accept source_snapshot, else
// a snapshot-sourced create is wrongly rejected as "image_url is required".
func validateDiskSource(s CreateSpec) error {
	switch {
	case s.ImageURL == "" && s.SourceSnapshot == nil:
		return fmt.Errorf("exactly one of image_url or source_snapshot is required")
	case s.ImageURL != "" && s.SourceSnapshot != nil:
		return fmt.Errorf("image_url and source_snapshot are mutually exclusive")
	}
	return nil
}
