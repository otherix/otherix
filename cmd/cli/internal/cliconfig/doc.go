// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package cliconfig — design notes.
//
// # Why YAML, why this shape
//
// The on-disk format is а small YAML document с four top-level
// fields (apiVersion / kind / current-cluster / clusters). The
// shape is deliberately a transparent subset of kubectl's
// kubeconfig:
//
//   - apiVersion + kind sentinel — leaves room for а future
//     migration tool that dispatches on Kind and refuses unknown
//     apiVersion values rather than silently rewriting.
//   - one current-cluster pointer + a flat clusters list — mirrors
//     kubectl's contexts/current-context split с the user/auth
//     dimension folded в (we have only bearer tokens, not the
//     four-axis user × cluster × namespace × auth-provider matrix
//     kubectl supports).
//   - No nesting beyond clusters[]. Operators editing the file by
//     hand should see flat, obvious YAML.
//
// # Comparison to neighbouring tools
//
//   - kubectl: ~/.kube/config, YAML, contexts × users × clusters.
//     We borrow the home-relative path и the apiVersion / kind
//     sentinel; we collapse contexts into the cluster row because
//     identity in Otherix is one role per token, not per-tool.
//   - docker: ~/.docker/config.json, JSON, no current concept —
//     auth indexed by registry hostname. We borrow the home-relative
//     path but pick YAML for the hand-edit ergonomics и cluster
//     pointer для multi-environment workflows.
//   - aws: ~/.aws/{config,credentials}, INI, profile-driven с
//     AWS_PROFILE env. We borrow the env-var-overrides-default
//     pattern (our OTHERIX_API_TOKEN / OTHERIX_SERVER play the AWS
//     env-var role) but keep one file, not two.
//   - gcloud: ~/.config/gcloud, multi-file directory с named
//     configurations. We deliberately stay with а single file: the
//     credential surface is small enough that а directory's overhead
//     outweighs its flexibility.
//
// # Atomicity and permissions
//
// Save writes а sibling temp file, chmods к 0600, fsyncs, и
// renames onto the target. The renaming is atomic on the local
// filesystem; а crash during Save cannot leave half-written
// credentials. The parent directory is created с 0700 on first
// run. Hand-editors с looser umasks may end up at 0644 — Load
// does not enforce 0600 (would be aggressive), but а future
// `otherix config doctor` could warn.
//
// # Precedence
//
// Endpoint и token are resolved independently и in parallel —
// see Resolve. The two chains share the same shape: explicit flag
// > env var > selected cluster > fail. Cluster selection prefers
// --cluster (and surfaces ErrClusterNotFound on miss — no silent
// fall-through), else current-cluster, else "no cluster".
//
// This is the same precedence model kubectl uses for --context
// versus current-context: explicit flag is а one-shot override
// that does not roll back to the default on miss.
package cliconfig
