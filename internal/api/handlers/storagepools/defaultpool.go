// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

package storagepools

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/otherix/otherix/internal/api/validation"
	"github.com/otherix/otherix/internal/store"
)

// DefaultPoolStore is the storage surface the default-pool ensurer depends on.
// *etcdstore.Store satisfies it.
type DefaultPoolStore interface {
	ClusterSettings(ctx context.Context) (store.ClusterSetting, error)
	CreateStoragePool(ctx context.Context, arg store.CreateStoragePoolParams) (store.StoragePool, error)
}

// EnsureDefaultPoolsFunc returns a heartbeat.NodeReadyHook that auto-provisions
// the cluster default storage pool on each freshly-promoted node. It reads the
// configured default name from cluster_settings; a nil name is the documented
// opt-out (operator provisions pools manually) and the hook no-ops.
//
// The hook is fully best-effort: a transient CreateStoragePool failure is logged
// at WARN and skipped, and a duplicate (ErrStoragePoolNameExists) is the
// idempotent steady state. KNOWN LIMITATION: provisioning fires only on the
// pending/unreachable -> ready promotion (the PromoteHealthyNodes transition),
// so a transient create failure is NOT retried until that node re-promotes; the
// operator fallback is a manual `pool create`.
func EnsureDefaultPoolsFunc(st DefaultPoolStore, pathPrefix string, log *slog.Logger) func(ctx context.Context, ready []store.PromoteHealthyNodesRow) error {
	return func(ctx context.Context, ready []store.PromoteHealthyNodesRow) error {
		cs, err := st.ClusterSettings(ctx)
		if err != nil {
			return err
		}
		if cs.DefaultPoolName == nil {
			// Opt-out: no cluster default configured, nothing to provision.
			return nil
		}
		name := *cs.DefaultPoolName
		path := pathPrefix + name

		// Defence-in-depth: the derived path must stay inside the allowlist
		// prefix. A name carrying a path separator ("a/b") or a traversal
		// segment (".", "..") would escape the prefix, so reject it before any
		// create. ValidateStoragePoolName now rejects these at the config/API
		// edge, but the name is read here from etcd (where a legacy row may
		// predate that tightening, or be written by a path that bypasses the
		// edge), so this guard (filepath.Clean + prefix check) is the
		// authoritative one.
		cleanPrefix := filepath.Clean(pathPrefix) + "/"
		if strings.ContainsRune(name, '/') || name == "." || name == ".." ||
			!strings.HasPrefix(filepath.Clean(path)+"/", cleanPrefix) {
			log.ErrorContext(ctx, "default pool name escapes the allowlist prefix; skipping auto-provision",
				slog.String("default_pool_name", name),
				slog.String("path_prefix", pathPrefix))
			return nil
		}

		for _, node := range ready {
			// A dedicated gateway hosts no guest VMs. Owning a pool is exactly
			// what derives the hypervisor role (EffectiveRoles), so provisioning
			// a default pool here would silently make every gateway a co-located
			// hypervisor and expose it to VM placement it cannot satisfy. A
			// gateway that should also be a hypervisor gets a pool the explicit
			// way (join as an agent, or `pool create`), never by this auto-hook.
			if node.GatewayRole {
				log.DebugContext(ctx, "skipping default pool for gateway node",
					slog.String("node_id", node.ID.String()),
					slog.String("name", name))
				continue
			}
			_, err := st.CreateStoragePool(ctx, store.CreateStoragePoolParams{
				ID:     uuid.New(),
				NodeID: node.ID,
				Name:   name,
				Type:   validation.StoragePoolTypeLocalDir,
				Path:   path,
				Config: normaliseConfig(nil),
			})
			switch {
			case err == nil:
				log.InfoContext(ctx, "default pool auto-created",
					slog.String("node_id", node.ID.String()),
					slog.String("name", name),
					slog.String("path", path))
			case errors.Is(err, store.ErrStoragePoolNameExists):
				log.DebugContext(ctx, "default pool already present",
					slog.String("node_id", node.ID.String()),
					slog.String("name", name))
			default:
				log.WarnContext(ctx, "default pool auto-create failed; will retry on next promotion",
					slog.String("node_id", node.ID.String()),
					slog.String("name", name),
					slog.String("error", err.Error()))
			}
		}
		return nil
	}
}
