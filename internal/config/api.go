// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/otherix/otherix/internal/logger"
	"github.com/otherix/otherix/internal/store"
)

// APIConfig is the configuration for the otherix-api binary.
type APIConfig struct {
	Server       ServerConfig       `koanf:"server"`
	AgentServer  AgentServerConfig  `koanf:"agent_server"`
	AgentClient  AgentClientConfig  `koanf:"agent_client"`
	CPCert       CPCertConfig       `koanf:"cp_cert"`
	ClusterCA    ClusterCAConfig    `koanf:"cluster_ca"`
	ClusterJoin  ClusterJoinConfig  `koanf:"cluster_join"`
	Logger       logger.Config      `koanf:"logger"`
	Auth         AuthConfig         `koanf:"auth"`
	Console      ConsoleConfig      `koanf:"console"`
	Workers      WorkersConfig      `koanf:"workers"`
	Placement    PlacementConfig    `koanf:"placement"`
	StoragePools StoragePoolsConfig `koanf:"storage_pools"`
	Network      NetworkConfig      `koanf:"network"`
	Etcd         EtcdConfig         `koanf:"etcd"`
}

// NetworkConfig holds cluster overlay-network settings. All fields are
// SEED-ONLY: read once at first boot to seed cluster_settings
// first-writer-wins; thereafter every replica reads the etcd value and
// ignores this field. Defaults: OverlaySupernet 10.42.0.0/16,
// VNIRange 1000-65535, UnderlayMTU 1500. UnderlayMTU is the physical underlay
// MTU; the overlay inner MTU and otwg0 MTU derive from it.
type NetworkConfig struct {
	OverlaySupernet string         `koanf:"overlay_supernet"`
	VNIRange        VNIRangeConfig `koanf:"vni_range"`
	UnderlayMTU     int            `koanf:"underlay_mtu"`
}

// VNIRangeConfig bounds the overlay VXLAN VNI allocation range. Seeded into
// cluster_settings first-writer-wins at bootstrap; immutable thereafter.
type VNIRangeConfig struct {
	Min int `koanf:"min"`
	Max int `koanf:"max"`
}

// minUnderlayMTU floors the seedable underlay MTU. The overlay inner MTU derives
// as underlay - store.OverlayEncapOverhead; flooring the underlay at
// OverlayEncapOverhead + 1280 keeps that derived overlay MTU at or above the
// 1280-byte IPv6 minimum link MTU (RFC 8200). Derived from the store constant so
// the two bounds cannot drift; mirrors etcdstore.SeedUnderlayMTU.
const minUnderlayMTU = int(store.OverlayEncapOverhead) + 1280 // 1390

// Validate enforces the bootstrap-immutable overlay bounds on the local config
// before it is seeded into cluster_settings, so a first-boot typo fails fast at
// load time rather than deep in the store seed. The bounds mirror
// etcdstore.SeedUnderlayMTU / SeedVNIRange: underlay_mtu is 0 (default 1500) or
// minUnderlayMTU..65535; vni_range is 0/0 (defaults) or 1000<=min<max<=16777215.
func (n NetworkConfig) Validate() error {
	if n.UnderlayMTU != 0 && (n.UnderlayMTU < minUnderlayMTU || n.UnderlayMTU > 65535) {
		return fmt.Errorf("network.underlay_mtu must be 0 (default) or in [%d, 65535] (got %d)", minUnderlayMTU, n.UnderlayMTU)
	}
	if n.VNIRange.Min == 0 && n.VNIRange.Max == 0 {
		return nil
	}
	if n.VNIRange.Min < 1000 || n.VNIRange.Max > 16777215 || n.VNIRange.Min >= n.VNIRange.Max {
		return fmt.Errorf("network.vni_range requires 1000 <= min < max <= 16777215 (got min=%d max=%d)", n.VNIRange.Min, n.VNIRange.Max)
	}
	return nil
}

// EtcdConfig configures the embedded etcd member that backs the
// control-plane store. The single-node defaults let a
// standalone api-server boot with no operator input; HA topologies
// (slice 9) override Mode + InitialCluster + the peer mTLS material.
//
// This is plain transport: cmd/api translates it into the leaf
// internal/etcd.Config, whose Validate is the single source of truth
// for the invariants (mode enum, required URLs, initial-cluster
// requirement per mode). It is therefore not re-validated in
// APIConfig.Validate.
type EtcdConfig struct {
	Mode           string `koanf:"mode"`            // single | bootstrap | join
	Name           string `koanf:"name"`            // unique member name within the cluster
	DataDir        string `koanf:"data_dir"`        // member data directory (WAL + snapshots)
	PeerURL        string `koanf:"peer_url"`        // Raft peer advertise/listen URL
	ClientURL      string `koanf:"client_url"`      // client advertise/listen URL
	ClusterToken   string `koanf:"cluster_token"`   // initial-cluster token; isolates clusters on shared networks
	InitialCluster string `koanf:"initial_cluster"` // full member list "n0=peer0,n1=peer1,..." (bootstrap|join)
	PeerCertFile   string `koanf:"peer_cert_file"`  // operator-provided peer (Raft) mTLS leaf cert (used verbatim when set)
	PeerKeyFile    string `koanf:"peer_key_file"`   // operator-provided peer (Raft) mTLS leaf key
	PeerCAFile     string `koanf:"peer_ca_file"`    // operator-provided cluster CA trust anchor for peer mTLS
	PeerAutoDir    string `koanf:"peer_auto_dir"`   // directory the auto-generated peer cert/key/ca land in when no operator override is set

	CompactionMode      string `koanf:"compaction_mode"`      // periodic | revision (default periodic)
	CompactionRetention string `koanf:"compaction_retention"` // duration "1h" (periodic) or count "5000" (revision); default "1h"
}

// ClusterCAConfig pins the on-disk location of the cluster CA (cert +
// key). Under the HA peer-PKI model the CA is a filesystem artifact: it
// is provisioned to disk before etcd starts because the peer (Raft) mTLS
// plane needs a CA-signed cert pre-start, and it lives on every replica
// so each signs its own peer cert locally.
//
// A bootstrap / single node generates the CA here on first boot and
// reloads it on restart; a join node receives it via the replica-join
// protocol before its member starts. Defaults to
// /opt/otherix/ca/cluster-ca.{crt,key}.
type ClusterCAConfig struct {
	CertFile string `koanf:"cert_file"`
	KeyFile  string `koanf:"key_file"`
}

// Validate enforces that the CA cert and key paths are both set: the
// on-disk CA provisioning needs both, and an empty path is an operator
// error rather than a meaningful default (defaultAPIConfig always
// populates them).
func (c ClusterCAConfig) Validate() error {
	if c.CertFile == "" || c.KeyFile == "" {
		return errors.New("cluster_ca.cert_file and cluster_ca.key_file must both be set")
	}
	return nil
}

// ClusterJoinConfig drives the joiner-side cluster CA fetch, consulted only
// when etcd.mode is "join" and no cluster CA is yet on disk. The replica
// redeems a kind=cluster join token at an existing replica's
// /v1/cluster/join (CPURL) and persists the returned CA cert + key. CAFingerprint
// pins the expected CA out of band (TOFU). The token is supplied inline
// (Token) or read from TokenPath (preferred - keeps the secret out of the
// config file).
type ClusterJoinConfig struct {
	CPURL         string        `koanf:"cp_url"`
	Token         string        `koanf:"token"`
	TokenPath     string        `koanf:"token_path"`
	CAFingerprint string        `koanf:"ca_fingerprint"`
	Timeout       time.Duration `koanf:"timeout"`
}

// StoragePoolsConfig pins operator-facing knobs for `POST
// /v1/storage-pools`. The path allowlist constrains what filesystem
// prefixes a pool's `path` field can target — defence in depth against
// operator typo / hostile create that points at /etc/. Default
// `/opt/otherix/pools/` matches the filesystem convention in CLAUDE.md;
// operators widening the allowlist (NFS mountpoints, custom locations)
// override at deploy time.
type StoragePoolsConfig struct {
	AllowedPathPrefixes []string `koanf:"allowed_path_prefixes"`
}

// CPCertConfig configures the per-replica CP server cert lifecycle.
// Three operational modes — operator override, local cache,
// auto-generate (default). Each replica selects a mode at boot via
// LoadOrGenerateCPCert orchestrator.
//
// Mode A (operator override) — CertFile + KeyFile both set + files
// exist on disk → loaded as the replica's external cert (Let's
// Encrypt, corporate CA, FIPS scenarios). Missing files when paths
// configured = fatal.
//
// Mode B (local cache) — LocalCache.Enabled = true → cached cert
// reused across restarts when still valid (chain to current CA,
// NotAfter > now + 30d buffer, SAN superset of expected). Cache
// path defaults to /opt/otherix/certs/cp-cert.{crt,key}.
//
// Mode C (auto-generate) — fresh ECDSA P-384 leaf signed by cluster
// CA on every boot, in-memory only. Default mode. SAN composition =
// auto-detected baseline (localhost, 127.0.0.1, hostname, non-
// wildcard listen address) ∪ AdditionalSANs.
//
// Cluster CA always loaded from DB (no `ca_file` knob here OR in the
// agent_server / agent_client blocks — those legacy fields removed
// per pre-prod squash window).
type CPCertConfig struct {
	CertFile       string            `koanf:"cert_file"`
	KeyFile        string            `koanf:"key_file"`
	LocalCache     CPCertCacheConfig `koanf:"local_cache"`
	AdditionalSANs []string          `koanf:"additional_sans"`
	Validity       time.Duration     `koanf:"validity"`
}

// CPCertCacheConfig pins the local-disk cache feature (Mode B).
// Enabled = true → generated cert persisted to (CertPath, KeyPath)
// after generation; valid cache reused on subsequent boots. Default
// paths apply when Enabled = true but explicit paths are absent.
type CPCertCacheConfig struct {
	Enabled  bool   `koanf:"enabled"`
	CertPath string `koanf:"cert_path"`
	KeyPath  string `koanf:"key_path"`
}

// Validate enforces the cp_cert invariants. Mode A: cert_file and
// key_file must both be set OR both unset (paired). Mode B: cache
// paths similarly paired. Validity > 0 (defaults applied for zero
// values at orchestrator entry); minimum 24h enforced to prevent
// operationally-useless lifetimes.
//
// AdditionalSANs entries are not validated here — net.ParseIP /
// DNS classification happens at SAN composition time, and malformed
// entries surface at cert generation (x509.CreateCertificate
// rejects unparseable names).
func (c CPCertConfig) Validate() error {
	if (c.CertFile == "") != (c.KeyFile == "") {
		return errors.New("cp_cert.cert_file and cp_cert.key_file must both be set or both unset")
	}
	if (c.LocalCache.CertPath == "") != (c.LocalCache.KeyPath == "") {
		return errors.New("cp_cert.local_cache.cert_path and .key_path must both be set or both unset")
	}
	if c.Validity != 0 && c.Validity < 24*time.Hour {
		return fmt.Errorf("cp_cert.validity must be >= 24h when set, got %v", c.Validity)
	}
	return nil
}

// PlacementConfig selects the VM placement algorithm and per-resource
// gating. The choice is passed down to internal/scheduler.SchedulePlacement
// at vm.create time. Placement runs in-process inside the api-server and
// serializes across replicas via store.LockKeyPlacement — no separate
// scheduler binary.
//
// Accepted Algorithm values mirror the scheduler.Algorithm* constants:
//
//   - "resource_aware" (default) — kubernetes-style LeastAllocated
//     scoring across enabled resources with graceful fallback for nodes
//     whose heartbeat / scan metrics have not landed.
//   - "least_vm_count" — legacy algorithm, minimum pinned-VM count
//     wins. Operator opt-out path; both algorithms use the same
//     name-lexicographic tie-break.
//
// Resources gates which dimensions (CPU, memory, disk) participate in
// the fit check + scoring and assigns each a post-effective overcommit
// multiplier. Defaults to strict every-dimension placement.
//
// Pressure configures node-pressure detection. Pressure is computed
// CP-side at heartbeat receipt: nodes exceeding a condition's
// threshold for ConsecutiveRequired observations get flagged and
// excluded from placement until recovery. Memory pressure is computed
// from heartbeat metrics; SystemDisk and Disk from periodic scan
// metrics.
type PlacementConfig struct {
	Algorithm string          `koanf:"algorithm"`
	Resources ResourcesConfig `koanf:"resources"`
	Pressure  PressureConfig  `koanf:"pressure"`
}

// ResourcesConfig groups the per-resource placement-time knobs. Each
// resource is configured independently; disabling one drops it from
// both the fit check and the score, and if every resource is disabled the
// scoring degrades to count-based (Least-VM-count parity).
type ResourcesConfig struct {
	CPU    ResourceConfig `koanf:"cpu"`
	Memory ResourceConfig `koanf:"memory"`
	Disk   ResourceConfig `koanf:"disk"`
}

// ResourceConfig configures placement consideration of a single
// resource dimension.
//
// Enabled toggles whether the resource participates in fit check +
// scoring. When false, the resource is skipped entirely (no
// constraint, no contribution to the LeastAllocated score). Defaults
// to true.
//
// OvercommitRatio multiplies the effective availability for the
// resource during placement decisions. Values > 1.0 enable overcommit
// (capacity inflated); values < 1.0 reserve headroom (capacity
// deflated). Defaults to 1.0 (strict — no overcommit). Validation
// requires the value to be strictly positive even when Enabled=false
// so the configuration stays hygienic against a future toggle.
type ResourceConfig struct {
	Enabled         bool    `koanf:"enabled"`
	OvercommitRatio float64 `koanf:"overcommit_ratio"`
}

// PressureConfig groups per-condition node-pressure detection knobs.
// Each pressure type is configured independently and computed CP-side
// from heartbeat / scan metrics; the scheduler excludes flagged nodes
// (or pools, for disk pressure) from placement until recovery.
//
// Sub-iteration A shipped Memory pressure (heartbeat-driven, per-node).
// Sub-iteration B adds SystemDisk pressure (heartbeat-driven, per-node,
// requires agent root-filesystem metrics in the heartbeat payload) and
// Disk pressure (scan-driven, per-pool — a pressured pool is excluded
// independently of other pools on the same node).
type PressureConfig struct {
	Memory     PressureConditionConfig `koanf:"memory"`
	SystemDisk PressureConditionConfig `koanf:"system_disk"`
	Disk       PressureConditionConfig `koanf:"disk"`
}

// PressureConditionConfig pins detection for a single pressure type.
//
// Enabled toggles detection. When false, the condition never triggers
// regardless of metrics — useful for clusters where the operator wants
// scheduling decisions decoupled from resource pressure altogether.
//
// ThresholdPercent triggers pressure when the resource's percentage
// available falls below this value. Lower = more pressure-sensitive.
// Range [1, 99] enforced by Validate.
//
// ConsecutiveRequired specifies how many consecutive heartbeat
// observations below threshold must occur before the pressure flag is
// set. Recovery is asymmetric — a single observation at-or-above the
// threshold immediately clears the flag. >= 1 enforced by Validate.
type PressureConditionConfig struct {
	Enabled             bool `koanf:"enabled"`
	ThresholdPercent    int  `koanf:"threshold_percent"`
	ConsecutiveRequired int  `koanf:"consecutive_required"`
}

// defaultPressureConfig produces the production-default pressure
// configuration:
//
//   - Memory: enabled, 10% threshold, 3 consecutive heartbeats to set
//     (≈ 90 s at the default 30 s cadence). Asymmetric — single
//     at-or-above-threshold observation clears.
//   - SystemDisk: same parameters as memory — root filesystem health
//     is similarly critical and heartbeat-driven.
//   - Disk (per-pool): 15% threshold (slightly more conservative —
//     sparse qcow2 growth absorbs space inside the window), and
//     consecutive_required=1 since scan cadence is minutes apart
//     and a single observation is sufficient to set.
func defaultPressureConfig() PressureConfig {
	return PressureConfig{
		Memory: PressureConditionConfig{
			Enabled:             true,
			ThresholdPercent:    10,
			ConsecutiveRequired: 3,
		},
		SystemDisk: PressureConditionConfig{
			Enabled:             true,
			ThresholdPercent:    10,
			ConsecutiveRequired: 3,
		},
		Disk: PressureConditionConfig{
			Enabled:             true,
			ThresholdPercent:    15,
			ConsecutiveRequired: 1,
		},
	}
}

// Validate enforces the per-condition invariants. Called from
// PlacementConfig.Validate so the api binary fails fast at startup on
// out-of-range thresholds or non-positive debounce windows.
func (p PressureConfig) Validate() error {
	if err := p.Memory.validate("memory"); err != nil {
		return err
	}
	if err := p.SystemDisk.validate("system_disk"); err != nil {
		return err
	}
	return p.Disk.validate("disk")
}

func (p PressureConditionConfig) validate(name string) error {
	if p.ThresholdPercent < 1 || p.ThresholdPercent > 99 {
		return fmt.Errorf("placement.pressure.%s.threshold_percent must be in [1, 99] (got %d)", name, p.ThresholdPercent)
	}
	if p.ConsecutiveRequired < 1 {
		return fmt.Errorf("placement.pressure.%s.consecutive_required must be >= 1 (got %d)", name, p.ConsecutiveRequired)
	}
	return nil
}

// defaultResourcesConfig produces the production-default per-resource
// configuration: every resource enabled, ratio 1.0 (strict). Operators
// who explicitly want overcommit set the ratio in config; operators
// who want to skip a resource flip Enabled=false.
func defaultResourcesConfig() ResourcesConfig {
	return ResourcesConfig{
		CPU:    ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
		Memory: ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
		Disk:   ResourceConfig{Enabled: true, OvercommitRatio: 1.0},
	}
}

// Validate gates the algorithm string + per-resource ratios at
// startup. Empty algorithm falls to "resource_aware" silently — operators
// dropping a partial config block see the default behaviour. Per-resource
// OvercommitRatio must be strictly positive even when Enabled=false
// (config hygiene against a future toggle).
func (p PlacementConfig) Validate() error {
	switch p.Algorithm {
	case "", "resource_aware", "least_vm_count":
	default:
		return fmt.Errorf("placement.algorithm must be \"resource_aware\" or \"least_vm_count\" (got %q)", p.Algorithm)
	}
	if err := p.Resources.Validate(); err != nil {
		return err
	}
	return p.Pressure.Validate()
}

// Validate enforces the per-resource invariants. Called from
// PlacementConfig.Validate so the api binary fails fast at startup on
// any negative-or-zero ratio.
func (r ResourcesConfig) Validate() error {
	if err := r.CPU.validate("cpu"); err != nil {
		return err
	}
	if err := r.Memory.validate("memory"); err != nil {
		return err
	}
	return r.Disk.validate("disk")
}

func (r ResourceConfig) validate(name string) error {
	if r.OvercommitRatio <= 0 {
		return fmt.Errorf("placement.resources.%s.overcommit_ratio must be > 0 (got %v)", name, r.OvercommitRatio)
	}
	return nil
}

// Warnings returns operator-facing diagnostics about placement
// configurations that pass Validate but warrant attention at startup:
// per-resource overcommit ratios > 1.0, extreme ratios > 2.0, and the
// all-resources-disabled scenario that degrades scoring to count-based
// fallback. Memory and disk carry resource-specific risk language (OOM
// kill / sparse-qcow2 disk-full). CPU overcommit is benign at typical
// homelab ratios so its warning stays neutral. Empty result for the
// production-default (every dimension enabled at ratio 1.0).
//
// Surfaced by cmd/api/main.go at slog Warn level on each startup;
// operators see the same line on every restart, which doubles as a
// reminder of the trade-off they opted into.
func (p PlacementConfig) Warnings() []string {
	const link = "see docs/scheduler-configuration.md"
	var out []string
	appendOvercommit := func(name string, r ResourceConfig, risk string) {
		if !r.Enabled || r.OvercommitRatio <= 1.0 {
			return
		}
		prefix := fmt.Sprintf("placement.resources.%s.overcommit_ratio=%.2f", name, r.OvercommitRatio)
		severity := "overcommit enabled"
		if r.OvercommitRatio > 2.0 {
			severity = "extreme overcommit (>2.0)"
		}
		if risk == "" {
			out = append(out, fmt.Sprintf("%s — %s; %s", prefix, severity, link))
			return
		}
		out = append(out, fmt.Sprintf("%s — %s (%s); %s", prefix, severity, risk, link))
	}
	appendOvercommit("cpu", p.Resources.CPU, "")
	appendOvercommit("memory", p.Resources.Memory, "OOM kill risk under memory pressure")
	appendOvercommit("disk", p.Resources.Disk, "disk-full risk under sparse qcow2 growth")
	if !p.Resources.CPU.Enabled && !p.Resources.Memory.Enabled && !p.Resources.Disk.Enabled {
		out = append(out, fmt.Sprintf("placement.resources: all dimensions disabled — scheduler degrades to count-based scoring; %s", link))
	}
	return out
}

// AuthConfig holds JWT signing material and token lifetimes.
type AuthConfig struct {
	JWTSecret     string        `koanf:"jwt_secret"`
	JWTAccessTTL  time.Duration `koanf:"jwt_access_ttl"`
	JWTRefreshTTL time.Duration `koanf:"jwt_refresh_ttl"`
}

// Validate enforces the minimum-viable auth config: a 32-byte JWT secret
// (HS256 minimum), and positive TTLs. The api binary calls this at start.
func (a AuthConfig) Validate() error {
	if len(a.JWTSecret) < 32 {
		return fmt.Errorf("auth.jwt_secret must be at least 32 bytes (got %d)", len(a.JWTSecret))
	}
	if a.JWTAccessTTL <= 0 {
		return errors.New("auth.jwt_access_ttl must be > 0")
	}
	if a.JWTRefreshTTL <= 0 {
		return errors.New("auth.jwt_refresh_ttl must be > 0")
	}
	return nil
}

// ConsoleConfig selects how the API server bridges VM console access.
type ConsoleConfig struct {
	AccessMode string `koanf:"access_mode"`
}

// WorkersConfig controls the in-process River worker pool embedded in the
// API server.
type WorkersConfig struct {
	Enabled         bool                   `koanf:"enabled"`
	MaxWorkers      int                    `koanf:"max_workers"`
	Tasks           TasksWorkersConfig     `koanf:"tasks"`
	Heartbeat       HeartbeatWorkersConfig `koanf:"heartbeat"`
	StoragePoolScan StoragePoolScanConfig  `koanf:"storage_pool_scan"`
	Backup          BackupConfig           `koanf:"backup"`
}

// BackupConfig pins the periodic etcd snapshot worker. When Enabled (and Dir is
// set) the api-server snapshots its member at Interval into Dir, keeping the
// newest Retention files. Disabled by default: a backup needs an operator-chosen
// destination, so it is opt-in rather than silently writing somewhere.
type BackupConfig struct {
	Enabled   bool          `koanf:"enabled"`
	Interval  time.Duration `koanf:"interval"`
	Dir       string        `koanf:"dir"`
	Retention int           `koanf:"retention"`
}

// Validate fails fast on a backup config that would silently not back up:
// enabled with no destination dir is the worst failure mode (operator believes
// backups run; they do not). Also rejects negative interval / retention.
func (b BackupConfig) Validate() error {
	if b.Enabled && b.Dir == "" {
		return errors.New("workers.backup.dir is required when workers.backup.enabled is true")
	}
	if b.Interval < 0 {
		return errors.New("workers.backup.interval must be >= 0")
	}
	if b.Retention < 0 {
		return errors.New("workers.backup.retention must be >= 0")
	}
	return nil
}

// StoragePoolScanConfig pins the periodic `storage_pool.scan_trigger`
// worker (disk-aware scheduling Sub-iteration A). The trigger fires at
// Interval, enumerates active pools whose owning node is scannable, and
// enqueues a fresh `storage_pool.scan` task for each — staggered by a
// random delay in [0, Jitter) to spread agent-side load.
//
// When Enabled is false the periodic schedule is not registered at
// startup; on-demand `POST /v1/storage-pools/{id}/scan` continues to
// work either way.
type StoragePoolScanConfig struct {
	Enabled  bool          `koanf:"enabled"`
	Interval time.Duration `koanf:"interval"`
	Jitter   time.Duration `koanf:"jitter"`
}

// Validate gates the interval / jitter pair at startup. Zero values
// fall through to package defaults at registration time; here we only
// reject configurations that would be operationally surprising
// (negative durations, jitter wider than interval).
func (s StoragePoolScanConfig) Validate() error {
	if s.Interval < 0 {
		return errors.New("workers.storage_pool_scan.interval must be >= 0")
	}
	if s.Jitter < 0 {
		return errors.New("workers.storage_pool_scan.jitter must be >= 0")
	}
	if s.Interval > 0 && s.Jitter >= s.Interval {
		return fmt.Errorf(
			"workers.storage_pool_scan.jitter (%s) must be less than interval (%s)",
			s.Jitter, s.Interval,
		)
	}
	return nil
}

// HeartbeatWorkersConfig groups the CP-side node-health reconciler
// knobs. The reconciler fires on a fixed cadence (Interval) and flips
// nodes between 'ready' and 'unreachable' based on whether their
// last_heartbeat_at falls inside or outside the StaleThreshold window.
// A node in 'unreachable' whose last heartbeat is older than the
// GoneGrace window (which must exceed StaleThreshold) is then advanced
// to the terminal 'gone' status. All fields fall back to package
// defaults when zero.
type HeartbeatWorkersConfig struct {
	StaleThreshold time.Duration `koanf:"stale_threshold"`
	GoneGrace      time.Duration `koanf:"gone_grace"`
	Interval       time.Duration `koanf:"interval"`
}

// TasksWorkersConfig groups configuration for the in-process workers
// that act on the `tasks` resource. Currently ships only the cleanup
// worker; future tasks-domain workers (per-type retention overrides,
// stuck-task detection, etc.) extend this struct.
type TasksWorkersConfig struct {
	Retention TaskRetentionConfig `koanf:"retention"`
}

// TaskRetentionConfig is the per-state retention window the
// tasks.cleanup worker uses. Zero values fall back to the package
// defaults at registration time, so an unset block behaves identically
// to the documented defaults — the schema is here for operators who
// need longer audit windows.
type TaskRetentionConfig struct {
	Completed time.Duration `koanf:"completed"`
	Failed    time.Duration `koanf:"failed"`
}

func defaultAPIConfig() APIConfig {
	return APIConfig{
		Server: ServerConfig{
			Listen:        "0.0.0.0:8080",
			ReadTimeout:   30 * time.Second,
			WriteTimeout:  30 * time.Second,
			ShutdownGrace: 30 * time.Second,
		},
		Logger:  logger.Config{Level: "info", Format: "json"},
		Auth:    AuthConfig{JWTAccessTTL: 15 * time.Minute, JWTRefreshTTL: 30 * 24 * time.Hour},
		Console: ConsoleConfig{AccessMode: "proxy"},
		Workers: WorkersConfig{
			Enabled:    true,
			MaxWorkers: 10,
			Tasks: TasksWorkersConfig{
				Retention: TaskRetentionConfig{
					Completed: 7 * 24 * time.Hour,
					Failed:    30 * 24 * time.Hour,
				},
			},
			Heartbeat: HeartbeatWorkersConfig{
				StaleThreshold: 90 * time.Second,
				GoneGrace:      5 * time.Minute,
				Interval:       30 * time.Second,
			},
			StoragePoolScan: StoragePoolScanConfig{
				Enabled:  true,
				Interval: 15 * time.Minute,
				Jitter:   30 * time.Second,
			},
			Backup: BackupConfig{
				Interval:  6 * time.Hour,
				Retention: 7,
			},
		},
		AgentClient: AgentClientConfig{
			Enabled:         false,
			Timeout:         5 * time.Minute,
			PollInterval:    1 * time.Second,
			PollMaxInterval: 30 * time.Second,
		},
		CPCert: CPCertConfig{
			Validity: 365 * 24 * time.Hour,
			LocalCache: CPCertCacheConfig{
				Enabled:  false,
				CertPath: "/opt/otherix/certs/cp-cert.crt",
				KeyPath:  "/opt/otherix/certs/cp-cert.key",
			},
		},
		Placement: PlacementConfig{
			Algorithm: "resource_aware",
			Resources: defaultResourcesConfig(),
			Pressure:  defaultPressureConfig(),
		},
		ClusterCA: ClusterCAConfig{
			CertFile: "/opt/otherix/ca/cluster-ca.crt",
			KeyFile:  "/opt/otherix/ca/cluster-ca.key",
		},
		StoragePools: StoragePoolsConfig{
			AllowedPathPrefixes: []string{"/opt/otherix/pools/"},
		},
		Network: NetworkConfig{
			OverlaySupernet: "10.42.0.0/16",
			VNIRange:        VNIRangeConfig{Min: 1000, Max: 65535},
			UnderlayMTU:     1500,
		},
		Etcd: EtcdConfig{
			Mode:         "single",
			Name:         "otherix-0",
			DataDir:      "/opt/otherix/etcd",
			PeerURL:      "https://127.0.0.1:2380",
			ClientURL:    "http://127.0.0.1:2379",
			ClusterToken: "otherix-cluster",
			PeerAutoDir:  "/opt/otherix/peer",
		},
	}
}

// Validate sanitises the allowlist — empty prefix list is a rejection
// of every path, which is a footgun on boot. Operators that want to
// allow `/` explicitly must list it. Trailing-slash invariant: every
// prefix must end with `/` so substring match `/opt/otherix/pools-evil/`
// does not match an `/opt/otherix/pools` prefix.
func (s StoragePoolsConfig) Validate() error {
	if len(s.AllowedPathPrefixes) == 0 {
		return errors.New("storage_pools.allowed_path_prefixes must contain at least one entry")
	}
	for _, p := range s.AllowedPathPrefixes {
		if p == "" {
			return errors.New("storage_pools.allowed_path_prefixes: empty prefix")
		}
		if p[0] != '/' {
			return fmt.Errorf("storage_pools.allowed_path_prefixes: %q must be absolute", p)
		}
		if p[len(p)-1] != '/' {
			return fmt.Errorf("storage_pools.allowed_path_prefixes: %q must end with '/'", p)
		}
	}
	return nil
}

// Validate checks the server listen address, agent server/client and
// CP cert config, auth config, console access mode, and the overlay
// network bounds (underlay_mtu / vni_range).
func (c APIConfig) Validate() error {
	if err := c.Server.Validate(); err != nil {
		return err
	}
	if err := c.AgentServer.Validate(); err != nil {
		return err
	}
	if err := c.AgentClient.Validate(); err != nil {
		return err
	}
	if err := c.CPCert.Validate(); err != nil {
		return err
	}
	if err := c.ClusterCA.Validate(); err != nil {
		return err
	}
	if err := c.Auth.Validate(); err != nil {
		return err
	}
	if c.Console.AccessMode != "proxy" && c.Console.AccessMode != "direct" {
		return errors.New(`console.access_mode must be "proxy" or "direct"`)
	}
	if err := c.Placement.Validate(); err != nil {
		return err
	}
	if err := c.Workers.StoragePoolScan.Validate(); err != nil {
		return err
	}
	if err := c.Workers.Backup.Validate(); err != nil {
		return err
	}
	if err := c.StoragePools.Validate(); err != nil {
		return err
	}
	if err := c.Network.Validate(); err != nil {
		return err
	}
	return nil
}
