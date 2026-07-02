// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/otherix/otherix/internal/logger"
)

// AgentConfig is the configuration for the otherix-agent binary.
//
// Agent identity is name-keyed:
//   - No NodeID field. Agent identity derives from the cert CN at
//     startup (`node-<name>` SubjectCN per auth.SignCSR).
//   - No Bootstrap field. Bootstrap is a standalone subcommand
//     (`otherix-agent bootstrap`) that reads flags directly; the serve
//     subcommand consumes only the runtime config below.
type AgentConfig struct {
	Server       ServerConfig       `koanf:"server"`
	Logger       logger.Config      `koanf:"logger"`
	StatePath    string             `koanf:"state_path"`
	ControlPlane ControlPlaneConfig `koanf:"control_plane"`
	TLS          TLSConfig          `koanf:"tls"`
	Migration    MigrationConfig    `koanf:"migration"`
	Artifacts    ArtifactsConfig    `koanf:"artifacts"`
	QEMU         QEMUConfig         `koanf:"qemu"`
	WireGuard    WireGuardConfig    `koanf:"wireguard"`
}

// QEMUConfig holds qemu-process tunables that vary per host.
type QEMUConfig struct {
	// AArch64FirmwarePath is the path to the UEFI firmware blob used to
	// boot aarch64 guests (e.g. /usr/share/AAVMF/AAVMF_CODE.fd, provided
	// by Debian/Ubuntu's qemu-efi-aarch64 package). aarch64 has no legacy
	// BIOS — without firmware no aarch64 guest can boot. Ignored on
	// amd64 hosts.
	AArch64FirmwarePath string `koanf:"aarch64_firmware_path"`
}

// WireGuardConfig holds the agent's WG fabric tunables. The keypair is
// generated lazily at serve (see internal/agent/wgkey); only the key path and
// the link/endpoint tunables live here.
type WireGuardConfig struct {
	// ListenPort is the UDP port the WG interface listens on.
	ListenPort int `koanf:"listen_port"`
	// PersistentKeepalive is the per-peer keepalive interval. Plumbed now;
	// consumed once peers exist (a later slice).
	PersistentKeepalive time.Duration `koanf:"persistent_keepalive"`
	// PrivateKeyPath is where the lazily-generated private key is persisted.
	PrivateKeyPath string `koanf:"private_key_path"`
	// AdvertisedEndpoint is the host:port the agent reports for peers to dial.
	// Empty is valid for a single-node fabric; required for mesh reachability.
	AdvertisedEndpoint string `koanf:"advertised_endpoint"`
}

// ControlPlaneConfig holds connection parameters for reaching the Otherix
// control plane from an agent node.
type ControlPlaneConfig struct {
	URL               string        `koanf:"url"`
	HeartbeatInterval time.Duration `koanf:"heartbeat_interval"`
}

// TLSConfig carries paths to the mTLS certificate material used by the
// agent's HTTPS server and outbound connections to the control plane.
type TLSConfig struct {
	CACertPath string `koanf:"ca_cert_path"`
	CertPath   string `koanf:"cert_path"`
	KeyPath    string `koanf:"key_path"`
}

// BootstrapConfig is the value struct consumed by `bootstrap.Bootstrap()`
// — populated inline by the `otherix-agent bootstrap` subcommand from
// CLI flags. No koanf binding; no env-var path. Bootstrap is driven via
// explicit CLI flags rather than a bootstrap.env file.
//
// Field semantics, mostly mirroring the public /v1/nodes/join contract:
//   - Token / TokenPath are mutually exclusive; exactly one must be set.
//   - CAFingerprint accepts "sha256:<64-hex>" or a bare 64-hex string
//     (case-insensitive; normalised to lowercase during validation).
//   - CPURL is the control-plane base URL; the bootstrap package
//     appends /v1/ca and /v1/nodes/join itself.
//   - Architecture is auto-detected from runtime.GOARCH at subcommand
//     entry (operators do not supply this).
//   - NodeName, AdvertisedEndpoint, MigrationHost,
//     MigrationPortRange{Start,End} mirror the request body fields
//     enforced by Step 2's API edge.
//   - RequestTimeout caps a single HTTP request (default 30s). Set
//     conservatively short on link-flaky networks since the bootstrap
//     does not retry.
type BootstrapConfig struct {
	Token              string
	TokenPath          string
	CAFingerprint      string
	CPURL              string
	NodeName           string
	Architecture       string
	AdvertisedEndpoint string
	// IngressAdvertisedEndpoint is the HTTPS URL clients dial to reach this
	// node's ingress splicer (/v1/connect), distinct from AdvertisedEndpoint
	// (the mTLS control endpoint). Optional here (length-capped when set); a
	// gateway node supplies it.
	IngressAdvertisedEndpoint string
	MigrationHost             string
	MigrationPortRangeStart   int
	MigrationPortRangeEnd     int
	RequestTimeout            time.Duration
}

// fingerprintHexPattern matches the canonical 64-char lowercase hex
// form (post-normalisation). Used by both BootstrapConfig.Validate and
// the bootstrap package's NormalizeFingerprint helper.
var fingerprintHexPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Validate enforces the bootstrap config invariants. Returns a single
// error describing the first failure encountered — the operator's
// remediation is to edit the config / env vars and retry.
//
// Implementation is split into per-group helpers so each stays under
// gocyclo's complexity ceiling and so future fields can extend the
// relevant group without touching the rest.
func (b *BootstrapConfig) Validate() error {
	if err := b.validateTokenSource(); err != nil {
		return err
	}
	if err := b.validateCAFingerprint(); err != nil {
		return err
	}
	if err := b.validateCPURL(); err != nil {
		return err
	}
	if err := b.validateIdentity(); err != nil {
		return err
	}
	return b.validateMigration()
}

func (b *BootstrapConfig) validateTokenSource() error {
	hasToken := b.Token != ""
	hasPath := b.TokenPath != ""
	switch {
	case hasToken && hasPath:
		return errors.New("bootstrap: token and token_path are mutually exclusive")
	case !hasToken && !hasPath:
		return errors.New("bootstrap: one of token or token_path is required")
	}
	return nil
}

func (b *BootstrapConfig) validateCAFingerprint() error {
	if b.CAFingerprint == "" {
		return errors.New("bootstrap: ca_fingerprint is required")
	}
	normalised := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(b.CAFingerprint), "sha256:"))
	if !fingerprintHexPattern.MatchString(normalised) {
		return fmt.Errorf("bootstrap: ca_fingerprint must be 64 hex chars (optional sha256: prefix), got %q", b.CAFingerprint)
	}
	return nil
}

func (b *BootstrapConfig) validateCPURL() error {
	if b.CPURL == "" {
		return errors.New("bootstrap: cp_url is required")
	}
	if !strings.HasPrefix(strings.ToLower(b.CPURL), "https://") {
		return fmt.Errorf("bootstrap: cp_url must use https scheme, got %q", b.CPURL)
	}
	return nil
}

func (b *BootstrapConfig) validateIdentity() error {
	if b.NodeName == "" {
		return errors.New("bootstrap: node_name is required")
	}
	if len(b.NodeName) > 253 {
		return errors.New("bootstrap: node_name must be at most 253 characters")
	}
	// Architecture is auto-detected from runtime.GOARCH at subcommand entry.
	// Defensive check stays so a caller that mints a BootstrapConfig
	// directly (tests, future integrations) does not pass an empty /
	// wrong value silently.
	switch b.Architecture {
	case "amd64", "arm64":
		// ok
	default:
		return fmt.Errorf("bootstrap: architecture must be amd64 or arm64, got %q", b.Architecture)
	}
	if b.AdvertisedEndpoint == "" {
		return errors.New("bootstrap: advertised_endpoint is required")
	}
	if len(b.AdvertisedEndpoint) > 2048 {
		return errors.New("bootstrap: advertised_endpoint must be at most 2048 characters")
	}
	if len(b.IngressAdvertisedEndpoint) > 2048 {
		return errors.New("bootstrap: ingress_advertised_endpoint must be at most 2048 characters")
	}
	return nil
}

func (b *BootstrapConfig) validateMigration() error {
	if b.MigrationHost == "" {
		return errors.New("bootstrap: migration_host is required")
	}
	if len(b.MigrationHost) > 253 {
		return errors.New("bootstrap: migration_host must be at most 253 characters")
	}
	if b.MigrationPortRangeStart < 1024 || b.MigrationPortRangeStart > 65535 ||
		b.MigrationPortRangeEnd < 1024 || b.MigrationPortRangeEnd > 65535 ||
		b.MigrationPortRangeEnd < b.MigrationPortRangeStart {
		return errors.New("bootstrap: migration_port_range must lie in [1024, 65535] with end >= start")
	}
	return nil
}

func defaultAgentConfig() AgentConfig {
	return AgentConfig{
		Server: ServerConfig{
			Listen:        "0.0.0.0:9443",
			ReadTimeout:   30 * time.Second,
			WriteTimeout:  30 * time.Second,
			ShutdownGrace: 30 * time.Second,
		},
		Logger:    logger.Config{Level: "info", Format: "json"},
		StatePath: "/var/lib/otherix/vms",
		ControlPlane: ControlPlaneConfig{
			HeartbeatInterval: 30 * time.Second,
		},
		Migration: MigrationConfig{
			PortRangeStart:     49152,
			PortRangeEnd:       49251,
			ConvergenceTimeout: 10 * time.Minute,
		},
		Artifacts: ArtifactsConfig{
			Root:           "/var/lib/otherix/artifacts",
			PortRangeStart: 49252,
			PortRangeEnd:   49351,
			ImageCache: ImageCacheConfig{
				MaxBytes:         50 << 30,
				MinFreeBytes:     10 << 30,
				EvictionInterval: 5 * time.Minute,
			},
			Scrub: ScrubConfig{
				Interval:            time.Hour,
				MinReverifyInterval: 168 * time.Hour,
				MaxBytesPerPass:     10 << 30,
			},
		},
		QEMU: QEMUConfig{
			AArch64FirmwarePath: "/usr/share/AAVMF/AAVMF_CODE.fd",
		},
		WireGuard: WireGuardConfig{
			ListenPort:          51820,
			PersistentKeepalive: 25 * time.Second,
			PrivateKeyPath:      "/var/lib/otherix/wg/private.key",
		},
	}
}

// Validate checks the server listen address, that control_plane.url is set,
// and that the migration port range is valid. Bootstrap config is no
// longer carried in the runtime config — the `otherix-agent bootstrap`
// subcommand validates its flags inline.
func (c AgentConfig) Validate() error {
	if err := c.Server.Validate(); err != nil {
		return err
	}
	if c.ControlPlane.URL == "" {
		return errors.New("control_plane.url is required")
	}
	if err := c.Migration.Validate(); err != nil {
		return err
	}
	return c.Artifacts.Validate()
}
