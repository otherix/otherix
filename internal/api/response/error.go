// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Andrei Taranik

// Package response provides HTTP-response helpers shared by every handler
// in the api-server. It centralises the error envelope, the
// stable ErrorCode catalog, and the JSON-write boilerplate so handlers
// never call http.Error or w.Write directly.
package response

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ErrorCode is a stable machine-readable identifier of an error. The
// cross-cutting catalog is mirrored from components/schemas/ErrorCode in
// api/openapi/control-plane.yaml. Endpoint-specific codes may be added
// alongside but must also land in the OpenAPI enum.
type ErrorCode string

// Cross-cutting error codes. Any addition here must also land in the
// ErrorCode enum in api/openapi/control-plane.yaml.
const (
	CodeUnauthenticated     ErrorCode = "unauthenticated"
	CodeInvalidCredentials  ErrorCode = "invalid_credentials" //nolint:gosec // not a credential, an error code
	CodeTokenRevoked        ErrorCode = "token_revoked"
	CodePermissionDenied    ErrorCode = "permission_denied"
	CodeNotFound            ErrorCode = "not_found"
	CodeMethodNotAllowed    ErrorCode = "method_not_allowed"
	CodeConflict            ErrorCode = "conflict"
	CodeResourceInUse       ErrorCode = "resource_in_use"
	CodeIdempotencyMismatch ErrorCode = "idempotency_key_mismatch"
	CodeValidationFailed    ErrorCode = "validation_failed"
	CodeRateLimited         ErrorCode = "rate_limited"
	CodeRequestTimeout      ErrorCode = "request_timeout"
	CodeAgentUnreachable    ErrorCode = "agent_unreachable"

	// Agent-passthrough vm.create error codes. The agent surfaces
	// these in the task error envelope; the CP relays them verbatim.
	CodeAgentTaskLost             ErrorCode = "agent_task_lost"
	CodeImportTimeout             ErrorCode = "import_timeout"
	CodeQcow2HeaderInvalid        ErrorCode = "qcow2_header_invalid"
	CodeChecksumMismatch          ErrorCode = "checksum_mismatch"
	CodeUnsupportedFormat         ErrorCode = "unsupported_format"
	CodePoolFull                  ErrorCode = "pool_full"
	CodePoolFilesystemUnsupported ErrorCode = "pool_filesystem_unsupported"
	CodeImageCollision            ErrorCode = "image_collision"
	CodeDownloadFailed            ErrorCode = "download_failed"
	// CodePathNotAllowed surfaces allowlist rejection: the requested
	// `path` field on POST /v1/storage-pools is not a prefix of any
	// configured `storage_pools.allowed_path_prefixes` entry. Recovery
	// is operator-side — widen the allowlist OR pick a path under one
	// of the existing prefixes.
	CodePathNotAllowed ErrorCode = "path_not_allowed"

	// vm.create / vm.delete error codes. The `pool_not_writable` /
	// `node_not_ready` / `node_not_found` group is CP-side validation;
	// `vm_create_failed` / `vm_delete_failed` collapse classifyVMError
	// fall-through; the agent passthrough code `qemu_spawn_failed` mirrors
	// what the agent surfaces on a failed qemu launch.
	CodeVMNotFound      ErrorCode = "vm_not_found"
	CodeVMNameInUse     ErrorCode = "vm_name_in_use"
	CodePoolNotWritable ErrorCode = "pool_not_writable"
	CodeNodeNotReady    ErrorCode = "node_not_ready"
	CodeNodeNotFound    ErrorCode = "node_not_found"
	CodeVMCreateFailed  ErrorCode = "vm_create_failed"
	CodeVMDeleteFailed  ErrorCode = "vm_delete_failed"
	CodeQemuSpawnFailed ErrorCode = "qemu_spawn_failed"

	// Placement / cluster-default error codes. Emitted by VM create
	// handler when the scheduler returns its sentinel errors or the
	// cluster has no default pool configured.
	CodePoolNotFound      ErrorCode = "pool_not_found"
	CodePoolNotOnNode     ErrorCode = "pool_not_on_node"
	CodeNoEligibleNodes   ErrorCode = "no_eligible_nodes"
	CodeDefaultPoolNotSet ErrorCode = "default_pool_not_set"
	CodePoolNameAmbiguous ErrorCode = "pool_name_ambiguous"

	// Console flow surface. vm_not_running gates `vms.console`
	// against non-running VMs; console_in_use surfaces the agent-side
	// per-VM single-session lock; protocol_not_available surfaces the
	// agent refusing a protocol the VM does not expose (e.g. serial
	// without `-serial unix:` chardev).
	CodeVMNotRunning         ErrorCode = "vm_not_running"
	CodeConsoleInUse         ErrorCode = "console_in_use"
	CodeProtocolNotAvailable ErrorCode = "protocol_not_available"

	// CodeNotImplemented marks an agent surface that is bound but not yet
	// functional (e.g. in-place snapshot revert, deferred to a later slice).
	// Returned with HTTP 501.
	CodeNotImplemented ErrorCode = "not_implemented"

	// Artifact-pool error codes.
	CodePoolRoleInvalid           ErrorCode = "pool_role_invalid"
	CodeArtifactPoolRequired      ErrorCode = "artifact_pool_required"
	CodeDefaultArtifactPoolNotSet ErrorCode = "default_artifact_pool_not_set"

	// CodeDefaultNetworkNotSet: GET /v1/cluster/default-network when the
	// cluster has no default network configured.
	CodeDefaultNetworkNotSet ErrorCode = "default_network_not_set"

	// SSH guest-cert mint error codes (POST /v1/vms/{id}/ssh-cert).
	// CodeSSHSessionRejected is the uniform anti-enumeration rejection: an
	// unknown VM, an unauthorized caller, and a bad/expired/revoked grant
	// all collapse to it so the endpoint leaks no VM existence.
	// CodeSSHLoginNotAllowed is the one non-uniform case: a grant caller who
	// already proved reach but requested a login other than the grant's
	// pinned login (no oracle, the caller is already authorized).
	CodeSSHSessionRejected ErrorCode = "ssh_session_rejected"
	CodeSSHLoginNotAllowed ErrorCode = "ssh_login_not_allowed"

	// CodeSSHIngressNotEnabled rejects a vm.create that requests SSH ingress
	// (ssh_ingress_enabled=true) when the feature is not available: the cluster
	// SSH-ingress master switch is explicitly disabled, or the cluster SSH
	// user-CA is not provisioned. The create fails loudly before any VM is made
	// rather than silently producing a VM that cannot accept an SSH-ingress login.
	CodeSSHIngressNotEnabled ErrorCode = "ssh_ingress_not_enabled"

	CodeInternal ErrorCode = "internal"
)

// contentTypeJSON is the wire Content-Type for every JSON body written by
// helpers in this package.
const contentTypeJSON = "application/json; charset=utf-8"

// ErrorBody is the root JSON shape of every 4xx and 5xx response.
type ErrorBody struct {
	Error ErrorDetails `json:"error"`
}

// ErrorDetails is the inner object of ErrorBody. Details is omitted when
// nil so callers do not need to allocate an empty map for the common case.
type ErrorDetails struct {
	Code    ErrorCode      `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// WriteError serialises the standard error envelope and writes it with
// the given HTTP status. Handlers and middleware must use this helper
// rather than http.Error to keep the wire format consistent.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code ErrorCode, message string, details map[string]any) {
	body := ErrorBody{
		Error: ErrorDetails{
			Code:    code,
			Message: message,
			Details: details,
		},
	}

	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Default().ErrorContext(r.Context(), "encoding error response failed", "error", err)
	}
}
