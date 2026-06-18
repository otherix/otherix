// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/otherix/otherix/internal/agent/artifactstore"
	blobshandlers "github.com/otherix/otherix/internal/agent/handlers/blobs"
	"github.com/otherix/otherix/internal/agent/heartbeat"
	"github.com/otherix/otherix/internal/agent/migration"
	"github.com/otherix/otherix/internal/agent/vm"
	"github.com/otherix/otherix/internal/config"
)

// setupBlobTransport brings the slice-C1 blob transport online: it asserts the
// production artifact store is present (fail closed), runs the one-time
// relocation sweep, and builds the serve/pull control handler. Returns the
// shared store (for the heartbeat blob inventory) and the handler.
func setupBlobTransport(cfg *config.AgentConfig, manager *vm.Manager, baseTLS *tls.Config, log *slog.Logger) (*artifactstore.Store, *blobshandlers.Handler, error) {
	artStore := manager.ArtifactStore()
	if artStore == nil {
		return nil, nil, fmt.Errorf("artifact store required in production (artifacts.root unset?)")
	}
	// One-time, best-effort relocation of disk-pool-resident snapshot blobs into
	// the artifact store. Fail-open: errors are logged inside, never fatal.
	manager.RelocateSnapshotsToStore()

	handler, err := buildBlobsHandler(cfg, manager, artStore, baseTLS, log)
	if err != nil {
		return nil, nil, err
	}
	return artStore, handler, nil
}

// buildBlobsHandler wires the slice-C1 blob control handler: the holder-side
// serve manager + the consumer-side puller. The serve listener reuses the
// cluster CA + node leaf from baseTLS and requires a cluster-CA node-leaf client
// cert (the Task-9 hardening boundary). The serve host is the migration/
// advertised host (the address a peer can reach this node on). Both control
// endpoints are mounted on the CP-only main server (only the CP calls serve /
// pull); the blob DATA path is the separate blobpeer listener.
func buildBlobsHandler(cfg *config.AgentConfig, manager *vm.Manager, artStore *artifactstore.Store, baseTLS *tls.Config, log *slog.Logger) (*blobshandlers.Handler, error) {
	blobPorts := migration.NewPortAllocator(cfg.Artifacts.PortRangeStart, cfg.Artifacts.PortRangeEnd)
	serveMgr, err := newBlobServeManager(artStore, blobPorts, cfg.Migration.Host, baseTLS, log)
	if err != nil {
		return nil, fmt.Errorf("serve manager: %w", err)
	}
	pullClient, err := newBlobPullClient(cfg.TLS)
	if err != nil {
		return nil, fmt.Errorf("pull client: %w", err)
	}
	return blobshandlers.New(serveMgr, blobPuller{manager: manager, client: pullClient}, log), nil
}

// blobPuller is the consumer-side half of the slice-C1 blob pull: it runs
// blobpeer.Pull (via the VM manager's tracked-task seam) so the work is a
// pollable agent task. It implements the blobs.BlobPuller seam. The mTLS client
// carries the node leaf cert (mutual TLS to the holder) and trusts the cluster
// CA.
type blobPuller struct {
	manager *vm.Manager
	client  *http.Client
}

// Pull starts a tracked agent task streaming the blob for digest from
// holderEndpoint into the local artifact store and returns the task id
// immediately. It implements blobs.BlobPuller.
func (p blobPuller) Pull(digest, token, holderEndpoint string) (string, error) {
	task, err := p.manager.PullBlob(context.Background(), p.client, digest, token, holderEndpoint)
	if err != nil {
		return "", err
	}
	return task.ID.String(), nil
}

// newBlobPullClient builds the mTLS *http.Client the consumer uses to dial a
// holder's peer serve listener: it presents the node leaf cert and trusts the
// cluster CA, the same trust the agent's main mTLS server uses. Mirrors
// heartbeat.buildTLSConfig.
func newBlobPullClient(cfg config.TLSConfig) (*http.Client, error) {
	if cfg.CertPath == "" || cfg.KeyPath == "" {
		return nil, fmt.Errorf("blob pull client: cert_path and key_path are required")
	}
	if cfg.CACertPath == "" {
		return nil, fmt.Errorf("blob pull client: ca_cert_path is required")
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("blob pull client: load cert/key: %v", err)
	}
	caBytes, err := os.ReadFile(cfg.CACertPath)
	if err != nil {
		return nil, fmt.Errorf("blob pull client: read ca: %v", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("blob pull client: ca cert %s contains no PEM blocks", cfg.CACertPath)
	}
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				Certificates: []tls.Certificate{cert},
				RootCAs:      caPool,
				MinVersion:   tls.VersionTLS13,
			},
		},
	}, nil
}

// blobInventoryAdapter implements heartbeat.BlobLister over the node's artifact
// store. NodeBlobs maps each artifactstore.BlobEntry to a heartbeat.BlobReport;
// it returns ok=false only on a List error so a transient enumeration failure
// never blocks the heartbeat liveness signal (and the CP preserves the prior
// inventory rather than clearing it, fail-closed). Mirrors poolImageAdapter.
type blobInventoryAdapter struct {
	store *artifactstore.Store
	log   *slog.Logger
}

// NodeBlobs returns the node-level blob inventory and true on success
// (including a genuinely empty inventory), or nil and false when the store could
// not be enumerated this tick.
func (a blobInventoryAdapter) NodeBlobs() ([]heartbeat.BlobReport, bool) {
	entries, err := a.store.List()
	if err != nil {
		a.log.Warn("heartbeat: node blob inventory unavailable", "error", err.Error())
		return nil, false
	}
	out := make([]heartbeat.BlobReport, 0, len(entries))
	for _, e := range entries {
		out = append(out, heartbeat.BlobReport{Digest: e.Digest, SizeBytes: e.SizeBytes})
	}
	return out, true
}
